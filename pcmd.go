package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Build and version are injected with the correct values at compile time. See
// LDFLAGS in the Makefile
var (
	Build   string
	Version string
)

func main() {
	config := parseConfig()
	maybeShowVersionAndExit(config)
	createAndChangeToWorkDir(config)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Instead of dying instantly when a signal is received, cancel the master
	// context. All blocking functions should respect ctx.Done() and return within
	// config.GracePeriod seconds after receiving the done signal.
	signals := make(chan os.Signal)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGHUP)

	go func() {
		// It would be nice if there was a way to handle the grace period logic at
		// the top level, but since the ProxyCommand is launched in a new process
		// group we can't simply exit this process without first cleaning up
		// children. Centralizing the grace period handeling would, therefore,
		// require centralizing child process management, which doesn't seem worth
		// it so far. For now, we simply trust that all blocking operations will
		// cooperate and correctly implement the grace period logic.
		sig := <-signals
		fmt.Fprintf(os.Stderr, "Received %s signal, giving processes %d seconds to clean up...\n", sig, config.GracePeriod)

		cancel()
	}()

	// Lock and run proxycommand, OR wait for the SSH ControlPath to exist.
	if config.ExpectControlMaster {
		lockOrExpectControlMaster(ctx, config)
		return
	}

	if config.Lock {
		lockOrExit(ctx, config)
		return
	}

	pipeToProxyCommand(ctx, config)
}

func lockOrExpectControlMaster(ctx context.Context, config Config) {
	unlock, locked := flockPath(config.LockFilePath)
	defer unlock()

	if locked {
		pipeToProxyCommand(ctx, config)
	} else {
		// Wait for control path, then try a nested ssh connection
		fmt.Fprintf(os.Stderr, "Waiting for SSH ControlMaster...\n")

		cancelLogTail, err := tailLogFileToStdErr(config)
		if err != nil {
			log.Fatal(err)
		}
		defer cancelLogTail()

		controlMasterIsUp := waitForControlMaster(config)
		cancelLogTail()

		if controlMasterIsUp {
			sshPort := strconv.Itoa(config.SSHPort)
			sshUserAtHost := fmt.Sprintf("%s@%s", config.SSHUser, config.SSHHost)
			proxyTarget := fmt.Sprintf("localhost:%d", config.SSHPort)
			cmd := exec.Command("ssh", "-W", proxyTarget, "-p", sshPort, sshUserAtHost)

			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			cmd.Start()
			cmd.Wait()
		} else {
			fmt.Fprintf(os.Stderr, "ControlMaster not detected\n")
			os.Exit(1)
		}
	}
}

func lockOrExit(ctx context.Context, config Config) {
	unlock, locked := flockPath(config.LockFilePath)
	defer unlock()

	if locked {
		pipeToProxyCommand(ctx, config)
	} else {
		log.Fatalf("Could not lock %s. Is there another session already in progress?", config.LockFilePath)
	}
}

func pipeToProxyCommand(ctx context.Context, config Config) {
	logFile := openLogFile(config)
	cancelLogTail, err := tailLogFileToStdErr(config)
	if err != nil {
		log.Fatal(err)
	}
	defer cancelLogTail()

	cmd := exec.Command(config.CmdName)
	cmd.Args = config.CmdArgs

	// Spawn child in a new process group so that it doesn't receive any SIGINTs
	// or SIGHUPs sent to this process. This process will forward a kill signal
	// after the grace period, if necessary.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
		Pgid:    0,
	}

	cmd.Stderr = logFile

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}

	err = cmd.Start()
	if err != nil {
		log.Fatal(err)
	}

	// The only reason that stdio are pipes rather than just directly set to
	// os.Stdin/os.Stdout is so that we can easily detect when either of the
	// streams closes. Thats important, because if one pipe closes, we want to
	// manually close the other to make sure the process doesn't get blocked
	// reading/writing to stdio once SSH has closed the connection.
	stdioDone := make(chan struct{})
	go func() {
		io.Copy(stdinPipe, os.Stdin)
		stdioDone <- struct{}{}
	}()

	go func() {
		io.Copy(os.Stdout, stdoutPipe)
		stdioDone <- struct{}{}
	}()

	cmdDone := make(chan struct{})
	go func() {
		cmd.Wait()
		cmdDone <- struct{}{}
		close(cmdDone)
	}()

	select {
	// If the command is done, do nothing.
	case <-cmdDone:
		return

	// If either of stdin or stdout finishes, assume that means proxying is done
	case <-stdioDone:
		goto CleanupAndWaitForGracePeriod

	// If context is canceled, also start the cleanup process
	case <-ctx.Done():
		goto CleanupAndWaitForGracePeriod
	}

	// Close both pipes (in case only one finished) and start grace period timer
	// to allow for process to do any cleanup. The command may continue to log to
	// stderr, but after this point, those will only go to the log file and not be
	// forwarded to the interactive terminal (because its hella annoying to have
	// things printed to a bash session after a command appears to be finished)
CleanupAndWaitForGracePeriod:
	cancelLogTail()

	stdinPipe.Close()
	stdoutPipe.Close()

	// Give process time to cleanup
	timeout := time.After(time.Duration(config.GracePeriod) * time.Second)

	select {
	case <-cmdDone:
		return
	case <-timeout:
		// Best effort cleanup
		cmd.Process.Kill()
		return
	}
}

func createAndChangeToWorkDir(config Config) {
	workDir := config.WorkDir
	filesPath := filepath.Join(workDir, ".pcmd")

	if err := os.MkdirAll(filesPath, 0755); err != nil {
		log.Fatal(err)
	}

	if err := os.Chdir(workDir); err != nil {
		log.Fatal(err)
	}
}

func maybeShowVersionAndExit(config Config) {
	if config.ShowVersion {
		fmt.Printf("%s (%s)\n", Version, Build)
		os.Exit(0)
	}
}

var noop = func() error {
	return nil
}

func flock(f *os.File) (unlock func() error, locked bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK {
		return noop, false, nil
	}

	if err != nil {
		return noop, false, err
	}

	unlock = func() error {
		return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}

	return unlock, true, nil
}

func flockPath(path string) (func() error, bool) {
	lockFile, err := os.OpenFile(path, os.O_TRUNC|os.O_CREATE, 0640)
	if err != nil {
		log.Fatal(err)
	}

	unlock, locked, err := flock(lockFile)
	if err != nil {
		log.Fatal(err)
	}

	unlockAndDelete := func() error {
		if locked {
			err := unlock()
			if err != nil {
				return err
			}
			return os.Remove(path)
		}

		return nil
	}

	return unlockAndDelete, locked
}

func openLogFile(config Config) *os.File {
	logFile, err := os.Create(config.LogFilePath)

	if err != nil {
		log.Fatal(err)
	}
	return logFile
}

// I assume all systems have the tail command. This seems easier than bringing
// in a new dependency to do this (or figuring out how to tail a file in go
// myself)
func tailLogFileToStdErr(config Config) (cancel func() error, err error) {
	cmd := exec.Command("tail", "-f", config.LogFilePath)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	if err != nil {
		return noop, err
	}

	cancel = func() error {
		return cmd.Process.Kill()
	}

	return cancel, nil
}

func waitForControlMaster(config Config) bool {
	for attempts := 0; attempts < 400; attempts++ {
		attempts++

		success := controlMasterIsUp(config)

		if success {
			return success
		}

		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func controlMasterIsUp(config Config) bool {
	sshUserAtHost := fmt.Sprintf("%s@%s", config.SSHUser, config.SSHHost)
	cmd := exec.Command("ssh", sshUserAtHost, "-O", "check")

	cmd.Start()
	cmd.Wait()

	exitCode := cmd.ProcessState.ExitCode()
	return (exitCode == 0)
}

// Config is the global config struct. It is validated and populated with
// parseConfig()
type Config struct {
	// Where to keep all working files. Default "."
	WorkDir string

	// Amount of time to give proxy command to perform any cleanup
	GracePeriod int

	// Only allow one instance of <PROXY-COMMAND> to run at one time
	Lock bool

	// These are required if locking. They are used to determine a unique path to
	// a lockfile (locked using the flock syscall). Locking can be explicitly
	// requested with the --lock flag, or is indirectly implied if using
	// --ssh-control-path. In almost all cases, they should map to the %r (remote
	// user), %h (remote host) and %p (remote port) tokens available to
	// ProxyCommand in ssh config. See SSH_CONFIG(5), specifically the
	// ProxyCommand and TOKENS sections.
	SSHUser string
	SSHHost string
	SSHPort int

	// If you are using `ControlMaster auto` in your SSH config, SSH does not take
	// any measures to only run one instance of the proxy command. SSH also
	// doesn't create the file specified with ControlPath until a connection is
	// fully established. That means if you SSH to the same host twice, and the
	// first invocation hasn't finished set up and created a master connection,
	// SSH will happily run two versions of your ProxyCommand in parallel. This
	// can be problematic under circumstances.
	//
	// pcmd can ensure that only one version of ProxyCommand runs at a time. If
	// pcmd detects that another version is running (determined using a lock file)
	// then pcmd will wait until the control master comes online, then retry the
	// SSH connection
	ExpectControlMaster bool

	//Show the current version of PCMD
	ShowVersion bool

	// Derived options, calculated during parseConfig()
	LockFilePath string
	LogFilePath  string

	// All the positional arguments that come after the flags. These represent the
	// underlying ProxyCommand to use.
	CmdArgs []string
	CmdName string
}

func parseConfig() Config {
	config := Config{}

	flagSet := flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	flagSet.StringVar(&config.WorkDir, "workdir", ".", "Working directory for lock files, logs, unix sockets, etc.")
	flagSet.IntVar(&config.GracePeriod, "grace-period", 300, "Number of seconds to allow for cleanup once the proxying is complete")
	flagSet.BoolVar(&config.Lock, "lock", false, "Only allow one instance of ProxyCommand to run at a time. Implied by -control-path")
	flagSet.StringVar(&config.SSHUser, "r", "", "The SSH remote user. This should be set to %r. See TOKENS in SSH_CONFIG(5) for more details.")
	flagSet.StringVar(&config.SSHHost, "h", "", "The SSH remote host. This should be set to %h. See TOKENS in SSH_CONFIG(5) for more details.")
	flagSet.IntVar(&config.SSHPort, "p", 22, "The SSH remote host. This should be set to %p. See TOKENS in SSH_CONFIG(5) for more details.")
	flagSet.BoolVar(&config.ExpectControlMaster, "wait-for-master", false, "If not the SSH master connection, pcmd will wait for the master to come up. Implies -lock.")
	flagSet.BoolVar(&config.ShowVersion, "version", false, "Show version")

	if err := flagSet.Parse(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
	remainingArgs := flagSet.Args()

	if len(remainingArgs) > 0 {
		config.CmdName = remainingArgs[0]
		config.CmdArgs = remainingArgs
	}

	baseFilename := baseName(config)

	// ExpectUpstream and ExpectControlPath imply Lock
	config.Lock = config.Lock || config.ExpectControlMaster

	config.LockFilePath = fmt.Sprintf("%s.lock", baseFilename)

	if config.Lock {
		ensureSSHConfigPresent(config)
		config.LogFilePath = fmt.Sprintf("%s.log", baseFilename)
	} else {
		// If we aren't locking, we should pick a LogFilePath path that is likely to
		// be unique. We only need to do this for LogFilePath since we don't use the
		// other paths when not locking. (and if we are locking, we don't need
		// unique names.)
		config.LogFilePath = fmt.Sprintf("%s.%s.%s.log", baseFilename, filepath.Base(config.CmdName), time.Now().Format("2006-01-02-15.04.05.999999999"))
	}

	return config
}

func baseName(config Config) (bn string) {
	bn = ".pcmd/pcmd"

	if config.SSHUser != "" {
		bn = bn + "." + config.SSHUser
	}

	if config.SSHHost != "" {
		bn = bn + "." + config.SSHHost
	}

	if config.SSHPort != 22 && config.SSHPort != 0 {
		bn = bn + "." + strconv.Itoa(config.SSHPort)
	}

	return bn
}

func ensureSSHConfigPresent(config Config) {
	if config.SSHUser == "" || config.SSHHost == "" || config.SSHPort == 0 {
		fmt.Fprintf(os.Stderr, "If using -lock or -wait-for-master, you muse also provide -r and -h (and -p if different than 22). pcmd uses this information to generate a unique path to a lock file.\n")
		os.Exit(1)
	}
}
