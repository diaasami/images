package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/osbuild/images/internal/cloud/awscloud"
)

// exitCheck can be deferred from the top of command functions to exit with an
// error code after any other defers are run in the same scope.
func exitCheck(err error) {
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error()+"\n")
		os.Exit(1)
	}
}

// createUserData creates cloud-init's user-data that contains user redhat with
// the specified public key
func createUserData(username, publicKeyFile string) (string, error) {
	publicKey, err := os.ReadFile(publicKeyFile)
	if err != nil {
		return "", err
	}

	userData := fmt.Sprintf(`#cloud-config
user: %s
ssh_authorized_keys:
  - %s
`, username, string(publicKey))

	return userData, nil
}

// resources created or allocated for an instance that can be cleaned up when
// tearing down.
type resources struct {
	AMI           *string `json:"ami,omitempty"`
	Snapshot      *string `json:"snapshot,omitempty"`
	SecurityGroup *string `json:"security-group,omitempty"`
	InstanceID    *string `json:"instance,omitempty"`
}

func run(c string, args ...string) ([]byte, []byte, error) {
	fmt.Printf("> %s %s\n", c, strings.Join(args, " "))
	cmd := exec.Command(c, args...)

	var cmdout, cmderr bytes.Buffer
	cmd.Stdout = &cmdout
	cmd.Stderr = &cmderr
	err := cmd.Run()

	// print any output even if the call failed
	stdout := cmdout.Bytes()
	if len(stdout) > 0 {
		fmt.Println(string(stdout))
	}

	stderr := cmderr.Bytes()
	if len(stderr) > 0 {
		fmt.Fprintf(os.Stderr, string(stderr)+"\n")
	}
	return stdout, stderr, err
}

func getInstanceType(arch string) (string, error) {
	switch arch {
	case "x86_64":
		return "t3.small", nil
	case "aarch64":
		return "t4g.medium", nil
	default:
		return "", fmt.Errorf("getInstanceType(): unknown architecture %q", arch)
	}
}

func sshRun(ip, user, key, hostsfile string, command ...string) error {
	sshargs := []string{"-i", key, "-o", fmt.Sprintf("UserKnownHostsFile=%s", hostsfile), "-l", user, ip}
	sshargs = append(sshargs, command...)
	_, _, err := run("ssh", sshargs...)
	if err != nil {
		return err
	}
	return nil
}

func scpFile(ip, user, key, hostsfile, source, dest string) error {
	_, _, err := run("scp", "-i", key, "-o", fmt.Sprintf("UserKnownHostsFile=%s", hostsfile), "--", source, fmt.Sprintf("%s@%s:%s", user, ip, dest))
	if err != nil {
		return err
	}
	return nil
}

func keyscan(ip, filepath string) error {
	var keys []byte
	maxTries := 30 // wait for at least 5 mins
	var keyscanErr error
	for try := 0; try < maxTries; try++ {
		keys, _, keyscanErr = run("ssh-keyscan", ip)
		if keyscanErr == nil {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if keyscanErr != nil {
		return keyscanErr
	}

	fmt.Printf("Creating known hosts file: %s\n", filepath)
	hostsFile, err := os.Create(filepath)
	if err != nil {
		return err
	}

	fmt.Printf("Writing to known hosts file: %s\n", filepath)
	if _, err := hostsFile.Write(keys); err != nil {
		return err
	}
	return nil
}

func newClientFromArgs(flags *pflag.FlagSet) (*awscloud.AWS, error) {
	region, err := flags.GetString("region")
	if err != nil {
		return nil, err
	}
	keyID, err := flags.GetString("access-key-id")
	if err != nil {
		return nil, err
	}
	secretKey, err := flags.GetString("secret-access-key")
	if err != nil {
		return nil, err
	}
	sessionToken, err := flags.GetString("session-token")
	if err != nil {
		return nil, err
	}

	return awscloud.New(region, keyID, secretKey, sessionToken)
}

func doSetup(a *awscloud.AWS, filename string, flags *pflag.FlagSet, res *resources) error {
	username, err := flags.GetString("username")
	if err != nil {
		return err
	}
	sshPubKey, err := flags.GetString("ssh-pubkey")
	if err != nil {
		return err
	}

	userData, err := createUserData(username, sshPubKey)
	if err != nil {
		return fmt.Errorf("createUserData(): %s", err.Error())
	}

	bucketName, err := flags.GetString("bucket")
	if err != nil {
		return err
	}
	keyName, err := flags.GetString("s3-key")
	if err != nil {
		return err
	}

	uploadOutput, err := a.Upload(filename, bucketName, keyName)
	if err != nil {
		return fmt.Errorf("Upload() failed: %s", err.Error())
	}

	fmt.Printf("file uploaded to %s\n", aws.StringValue(&uploadOutput.Location))

	var bootModePtr *string
	if bootMode, err := flags.GetString("boot-mode"); bootMode != "" {
		bootModePtr = &bootMode
	} else if err != nil {
		return err
	}

	imageName, err := flags.GetString("ami-name")
	if err != nil {
		return err
	}

	arch, err := flags.GetString("arch")
	if err != nil {
		return err
	}

	ami, snapshot, err := a.Register(imageName, bucketName, keyName, nil, arch, bootModePtr)
	if err != nil {
		return fmt.Errorf("Register(): %s", err.Error())
	}

	res.AMI = ami
	res.Snapshot = snapshot

	fmt.Printf("AMI registered: %s\n", aws.StringValue(ami))

	securityGroupName := fmt.Sprintf("image-boot-tests-%s", uuid.New().String())
	securityGroup, err := a.CreateSecurityGroupEC2(securityGroupName, "image-tests-security-group")
	if err != nil {
		return fmt.Errorf("CreateSecurityGroup(): %s", err.Error())
	}

	res.SecurityGroup = securityGroup.GroupId

	_, err = a.AuthorizeSecurityGroupIngressEC2(securityGroup.GroupId, "0.0.0.0/0", 22, 22, "tcp")
	if err != nil {
		return fmt.Errorf("AuthorizeSecurityGroupIngressEC2(): %s", err.Error())
	}

	instance, err := getInstanceType(arch)
	if err != nil {
		return err
	}
	runResult, err := a.RunInstanceEC2(ami, securityGroup.GroupId, userData, instance)
	if err != nil {
		return fmt.Errorf("RunInstanceEC2(): %s", err.Error())
	}
	instanceID := runResult.Instances[0].InstanceId
	res.InstanceID = instanceID

	ip, err := a.GetInstanceAddress(instanceID)
	if err != nil {
		return fmt.Errorf("GetInstanceAddress(): %s", err.Error())
	}
	fmt.Printf("Instance %s is running and has IP address %s\n", *instanceID, ip)
	return nil
}

func setup(cmd *cobra.Command, args []string) {
	var fnerr error
	defer func() { exitCheck(fnerr) }()

	filename := args[0]
	flags := cmd.Flags()

	a, err := newClientFromArgs(flags)
	if err != nil {
		fnerr = err
		return
	}

	// collect resources into res and write them out when the function returns
	resourcesFile, err := flags.GetString("resourcefile")
	if err != nil {
		fnerr = err
		return
	}
	res := &resources{}

	fnerr = doSetup(a, filename, flags, res)
	if fnerr != nil {
		fmt.Fprintf(os.Stderr, "setup() failed: %s\n", fnerr.Error())
		fmt.Fprint(os.Stderr, "tearing down resources\n")
		tderr := doTeardown(a, res)
		if tderr != nil {
			fmt.Fprintf(os.Stderr, "teardown(): %s\n", tderr.Error())
		}
	}

	resdata, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fnerr = fmt.Errorf("failed to marshal resources data: %s", err.Error())
		return
	}
	resfile, err := os.Create(resourcesFile)
	if err != nil {
		fnerr = fmt.Errorf("failed to create resources file: %s", err.Error())
		return
	}
	_, err = resfile.Write(resdata)
	if err != nil {
		fnerr = fmt.Errorf("failed to write resources file: %s", err.Error())
		return
	}
	fmt.Printf("IDs for any newly created resources are stored in %s. Use the teardown command to clean them up.\n", resourcesFile)
	if err = resfile.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "error closing resources file: %s\n", err.Error())
		fnerr = err
		return
	}
}

func doTeardown(aws *awscloud.AWS, res *resources) error {
	if res.InstanceID != nil {
		fmt.Printf("terminating instance %s\n", *res.InstanceID)
		if _, err := aws.TerminateInstanceEC2(res.InstanceID); err != nil {
			return fmt.Errorf("failed to terminate instance: %v", err)
		}
	}

	if res.SecurityGroup != nil {
		fmt.Printf("deleting security group %s\n", *res.SecurityGroup)
		if _, err := aws.DeleteSecurityGroupEC2(res.SecurityGroup); err != nil {
			return fmt.Errorf("cannot delete the security group: %v", err)
		}
	}

	if res.AMI != nil {
		fmt.Printf("deleting EC2 image %s and snapshot %s\n", *res.AMI, *res.Snapshot)
		if err := aws.DeleteEC2Image(res.AMI, res.Snapshot); err != nil {
			return fmt.Errorf("failed to deregister image: %v", err)
		}
	}
	return nil
}

func teardown(cmd *cobra.Command, args []string) {
	var fnerr error
	defer func() { exitCheck(fnerr) }()

	flags := cmd.Flags()

	a, err := newClientFromArgs(flags)
	if err != nil {
		fnerr = err
		return
	}

	resourcesFile, err := flags.GetString("resourcefile")
	if err != nil {
		return
	}

	res := &resources{}
	resfile, err := os.Open(resourcesFile)
	if err != nil {
		fnerr = fmt.Errorf("failed to open resources file: %s", err.Error())
		return
	}
	resdata, err := io.ReadAll(resfile)
	if err != nil {
		fnerr = fmt.Errorf("failed to read resources file: %s", err.Error())
		return
	}
	if err := json.Unmarshal(resdata, res); err != nil {
		fnerr = fmt.Errorf("failed to unmarshal resources data: %s", err.Error())
		return
	}

	fnerr = doTeardown(a, res)
}

func doRunExec(a *awscloud.AWS, filename string, flags *pflag.FlagSet, res *resources) error {
	privKey, err := flags.GetString("ssh-privkey")
	if err != nil {
		return err
	}

	username, err := flags.GetString("username")
	if err != nil {
		return err
	}

	tmpdir, err := os.MkdirTemp("", "boot-test-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	hostsfile := filepath.Join(tmpdir, "known_hosts")
	ip, err := a.GetInstanceAddress(res.InstanceID)
	if err != nil {
		return err
	}
	if err := keyscan(ip, hostsfile); err != nil {
		return err
	}

	// ssh into the remote machine and exit immediately to check connection
	if err := sshRun(ip, username, privKey, hostsfile, "exit"); err != nil {
		return err
	}

	// copy the executable without its path to the remote host
	destination := filepath.Base(filename)

	// copy the executable
	if err := scpFile(ip, username, privKey, hostsfile, filename, destination); err != nil {
		return err
	}

	// run the executable
	return sshRun(ip, username, privKey, hostsfile, fmt.Sprintf("./%s", destination))
}

func runExec(cmd *cobra.Command, args []string) {
	var fnerr error
	defer func() { exitCheck(fnerr) }()
	image := args[0]

	executable := args[1]
	flags := cmd.Flags()

	a, fnerr := newClientFromArgs(flags)
	if fnerr != nil {
		return
	}

	res := &resources{}
	defer func() {
		tderr := doTeardown(a, res)
		if tderr != nil {
			// report it but let the exitCheck() handle fnerr
			fmt.Fprintf(os.Stderr, "teardown(): %s\n", tderr.Error())
		}
	}()

	fnerr = doSetup(a, image, flags, res)
	if fnerr != nil {
		return
	}

	fnerr = doRunExec(a, executable, flags, res)
}

func setupCLI() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:                   "boot",
		Long:                  "upload and boot an image to the appropriate cloud provider",
		DisableFlagsInUseLine: true,
	}

	rootFlags := rootCmd.PersistentFlags()
	rootFlags.String("access-key-id", "", "access key ID")
	rootFlags.String("secret-access-key", "", "secret access key")
	rootFlags.String("session-token", "", "session token")
	rootFlags.String("region", "", "target region")
	rootFlags.String("bucket", "", "target S3 bucket name")
	rootFlags.String("s3-key", "", "target S3 key name")
	rootFlags.String("ami-name", "", "AMI name")
	rootFlags.String("arch", "", "arch (x86_64 or aarch64)")
	rootFlags.String("boot-mode", "", "boot mode (legacy-bios, uefi, uefi-preferred)")
	rootFlags.String("username", "", "name of the user to create on the system")
	rootFlags.String("ssh-pubkey", "", "path to user's public ssh key")
	rootFlags.String("ssh-privkey", "", "path to user's private ssh key")

	exitCheck(rootCmd.MarkPersistentFlagRequired("access-key-id"))
	exitCheck(rootCmd.MarkPersistentFlagRequired("secret-access-key"))
	exitCheck(rootCmd.MarkPersistentFlagRequired("region"))
	exitCheck(rootCmd.MarkPersistentFlagRequired("bucket"))

	// TODO: make it optional and use UUID if not specified
	exitCheck(rootCmd.MarkPersistentFlagRequired("s3-key"))

	// TODO: make it optional and use UUID if not specified
	exitCheck(rootCmd.MarkPersistentFlagRequired("ami-name"))

	exitCheck(rootCmd.MarkPersistentFlagRequired("arch"))

	// TODO: make it optional and use a default
	exitCheck(rootCmd.MarkPersistentFlagRequired("username"))

	// TODO: make ssh key pair optional for 'run' and if not specified generate
	// a temporary key pair
	exitCheck(rootCmd.MarkPersistentFlagRequired("ssh-privkey"))
	exitCheck(rootCmd.MarkPersistentFlagRequired("ssh-pubkey"))

	setupCmd := &cobra.Command{
		Use:                   "setup [--resourcefile <filename>] <filename>",
		Short:                 "upload and boot an image and save the created resource IDs to a file for later teardown",
		Args:                  cobra.ExactArgs(1),
		Run:                   setup,
		DisableFlagsInUseLine: true,
	}
	setupCmd.Flags().StringP("resourcefile", "r", "resources.json", "path to store the resource IDs")
	rootCmd.AddCommand(setupCmd)

	teardownCmd := &cobra.Command{
		Use:   "teardown [--resourcefile <filename>]",
		Short: "teardown (clean up) all the resources specified in a resources file created by a previous 'setup' call",
		Args:  cobra.NoArgs,
		Run:   teardown,
	}
	teardownCmd.Flags().StringP("resourcefile", "r", "resources.json", "path to store the resource IDs")
	rootCmd.AddCommand(teardownCmd)

	runCmd := &cobra.Command{
		Use:   "run <image> <executable>",
		Short: "upload and boot an image, then upload the specified executable and run it on the remote host",
		Args:  cobra.ExactArgs(2),
		Run:   runExec,
	}
	rootCmd.AddCommand(runCmd)

	return rootCmd
}

func main() {
	cmd := setupCLI()
	exitCheck(cmd.Execute())
}
