package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"newrelic"
	"newrelic/config"
	"newrelic/log"
	"newrelic/utilization"
	"newrelic/version"
)

// Program usage text. Note the options are organized in an attempt to
// place the most relevant choices near the top.
const usage = `Usage: newrelic-daemon [OPTIONS]

Options:

   -c <config-file>           Set the path to the configuration file
   --logfile <file>           Set the path to the log file
   --loglevel <level>         Log level (error, warning, info or debug)
                              Default: info
   --pidfile <file>           Set the path to the process id file
   --addr <addr>              Listen on the specified ip:port or socket file path
   --port <port>              Listen on the specified port or socket file path (deprecated, use --addr)
   --proxy <url>              Proxy credentials to use
   --auditlog <file>          Set the path to the audit file
   --cafile <file>            Set the path to root CA bundle
   --capath <dir>             Set the path to a directory of root CA certs
   --define <setting>=<value> Set a setting (as in newrelic.cfg) to a value
                              Uses the same syntax as newrelic.cfg
                              Takes precedence over config file settings
   -f, --foreground           Remain in the foreground
   -h, --help                 Print this message and exit
   -v, --version              Print version information and exit

Note: command line options have higher priority than their corresponding
option in the configuration file.

Please visit https://docs.newrelic.com/docs/agents/php-agent for additional help.
`

const legacyNotice = `Warning!

You are using legacy command-line flags. We plan to remove the following flags
in a future version:

[-p pidfile]
[-d level]
[-c config]
[-l logfile]
[-P port]
[-s]
[-n]
[-b SSL-certificate-bundle]
[-S SSL-certificate-path]
[-H host]
[-x proxy]
[-a auditlog]

Please use the shiny new flags listed with the -h or --help flag.
`

// A Role determines how the current daemon process should behave.
type Role int

const (
	// RoleProgenitor indicates a daemon process whose sole responsiblity is
	// to respawn itself in a new session and exit. This ensures the daemon
	// can outlive its original parent and does not have a controlling TTY.
	// When the agent spawns a daemon, the aforementioned is what we want
	// to happen. For that reason it is the default behavior.
	RoleProgenitor Role = iota

	// RoleWatcher indicates a daemon process that spawns and supervises workers.
	// When a worker exits unexpectedly, the watcher logs the error and
	// spawns a new worker.
	RoleWatcher

	// RoleWorker indicates a daemon process that creates and binds a listener,
	// collects data, and executes the harvest.
	RoleWorker
)

// Config provides the effective settings for the daemon.
type Config struct {
	BindAddr          string         `config:"addr"`                           // Listener bind address
	BindPort          string         `config:"port"`                           // Listener bind address, path=UDS, port=TCP
	Proxy             string         `config:"proxy"`                          // Proxy credentials to use for reporting
	Pidfile           string         `config:"pidfile"`                        // Path to daemon pid file
	NoPidfile         bool           `config:"-"`                              // Used to avoid two processes using pidfile
	LogFile           string         `config:"logfile"`                        // Path to daemon log file
	LogLevel          log.Level      `config:"loglevel"`                       // Log level
	AuditFile         string         `config:"auditlog"`                       // Path to audit log
	ConfigFile        string         `config:"-"`                              // Location of config file
	Foreground        bool           `config:"-"`                              // Remain in foreground
	Role              Role           `config:"-"`                              // This daemon's role
	Utilization       bool           `config:"-"`                              // Whether to print utilization data and exit
	DetectAWS         bool           `config:"utilization.detect_aws"`         // Whether to detect if this is running on AWS in utilization
	DetectAzure       bool           `config:"utilization.detect_azure"`       // Whether to detect if this is running on Azure in utilization
	DetectGCP         bool           `config:"utilization.detect_gcp"`         // Whether to detect if this is running on GCP in utilization
	DetectPCF         bool           `config:"utilization.detect_pcf"`         // Whether to detect if this is running on PCF in utilization
	DetectDocker      bool           `config:"utilization.detect_docker"`      // Whether to detect if this is in a Docker container in utilization
	LogicalProcessors int            `config:"utilization.logical_processors"` // Customer provided number of logical processors for pricing control.
	TotalRamMIB       int            `config:"utilization.total_ram_mib"`      // Customer provided total RAM in mebibytes for pricing control.
	BillingHostname   string         `config:"utilization.billing_hostname"`   // Customer provided hostname for pricing control.
	Agent             bool           `config:"-"`                              // Used to indicate if spawned by agent
	MaxFiles          uint64         `config:"rlimit_files"`                   // Maximum number of open file descriptors
	PProfPort         int            `config:"-"`                              // Port for pprof web server
	CAPath            string         `config:"ssl_ca_path"`                    // Path to a directory of root CA certificates.
	CAFile            string         `config:"ssl_ca_bundle"`                  // Path to a file containing a bundle of root CA certificates.
	IntegrationMode   bool           `config:"-"`                              // Whether to log integration test output
	AppTimeout        config.Timeout `config:"app_timeout"`                    // Inactivity timeout for applications.
}

func (cfg *Config) MakeUtilConfig() utilization.Config {
	return utilization.Config{
		DetectAWS:         cfg.DetectAWS,
		DetectAzure:       cfg.DetectAzure,
		DetectGCP:         cfg.DetectGCP,
		DetectPCF:         cfg.DetectPCF,
		DetectDocker:      cfg.DetectDocker,
		LogicalProcessors: cfg.LogicalProcessors,
		TotalRamMIB:       cfg.TotalRamMIB,
		BillingHostname:   cfg.BillingHostname,
	}
}

var (
	printVersion = false
)

var (
	exitStatus int
	exitMutex  sync.Mutex
)

func shouldCreatePidfile(cfg *Config) bool {
	if cfg.NoPidfile || "" == cfg.Pidfile {
		return false
	}
	return cfg.Role == RoleWatcher || cfg.Role == RoleWorker
}

// This helper function exists so that the pidfile cleanup defer will not be in
// the same function as the actual exit call.
func run(cfg *Config) {
	if shouldCreatePidfile(cfg) {
		pidFile, err := newrelic.CreatePidFile(cfg.Pidfile)
		if err != nil {
			if err == newrelic.ErrLocked {
				// another process has already holds the pid file lock
				return
			}

			log.Errorf("could not create pid file: %v", err)
			setExitStatus(1)
			return
		}
		defer pidFile.Remove()

		log.Debugf("pidfile=%s", pidFile.Name())

		if _, err := pidFile.Write(); err != nil {
			log.Errorf("could not write pid to file: %v", err)
			setExitStatus(1)
			return
		}
	}

	switch cfg.Role {
	case RoleProgenitor:
		if _, err := spawnWatcher(cfg); err != nil {
			// Some older RHEL 5.x linux kernels incorrectly handle missing system
			// calls (here: pipe2), which manifests as an EBADF error when spawning
			// a child process.
			if runtime.GOOS == "linux" {
				perr, ok := err.(*os.PathError)
				if ok && perr.Err == syscall.EBADF {
					err = borkedSyscallError("pipe2")
				}
			}

			log.Errorf("unable to create watcher process: %v", err)
			setExitStatus(1)
		}
	case RoleWatcher:
		runWatcher(cfg)
	case RoleWorker:
		runWorker(cfg)
	default:
		log.Errorf("invalid role: %d", cfg.Role)
		setExitStatus(1)
	}

}

func main() {
	// Respect GOMAXPROCS if set; otherwise, use all available CPUs.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	cfg := configure()

	if printVersion {
		fmt.Printf("New Relic daemon version %s\n", version.Full())
		fmt.Println("(C) Copyright 2009-2019 New Relic Inc. All rights reserved.")
		fmt.Println()
		return
	}

	if cfg.Utilization {
		util := utilization.Gather(utilization.Config{
			DetectAWS:    true,
			DetectAzure:  true,
			DetectGCP:    true,
			DetectPCF:    true,
			DetectDocker: true,
		})
		str, err := json.MarshalIndent(util, "", "\t")
		if err != nil {
			fmt.Printf("Error gathering utilization: %s", err.Error())
			os.Exit(1)
		}
		fmt.Printf("%s\n", str)

		os.Exit(0)
	}

	if err := initLog(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	log.Infof("%s", banner(cfg))
	for i := range os.Args {
		log.Debugf("ARGV[%d]: %s", i, os.Args[i])
	}
	log.Debugf("process role is %v", cfg.Role)

	run(cfg)
	os.Exit(exitStatus)
}

func createFlagSet(cfg *Config) *flag.FlagSet {
	flagSet := flag.NewFlagSet("", flag.ContinueOnError)
	// This prevents parsing errors from being printed. Instead, we'll print
	// those ourselves.
	flagSet.SetOutput(ioutil.Discard)

	// Print an empty string instead of the default usage if the initial flags
	// fail to parse. If it failed because of -h or -help flags, we'll print it
	// ourselves.
	flagSet.Usage = func() { fmt.Fprint(os.Stderr, "") }

	flagSet.StringVar(&cfg.ConfigFile, "c", cfg.ConfigFile, "config file location")
	flagSet.StringVar(&cfg.BindAddr, "addr", cfg.BindAddr, "")
	flagSet.StringVar(&cfg.BindPort, "port", cfg.BindPort, "")
	flagSet.StringVar(&cfg.Proxy, "proxy", cfg.Proxy, "")
	flagSet.StringVar(&cfg.Pidfile, "pidfile", cfg.Pidfile, "")
	flagSet.BoolVar(&cfg.NoPidfile, "no-pidfile", cfg.NoPidfile, "")
	flagSet.StringVar(&cfg.LogFile, "logfile", cfg.LogFile, "")
	flagSet.Var(&cfg.LogLevel, "loglevel", "LogLevel")
	flagSet.StringVar(&cfg.AuditFile, "auditlog", cfg.AuditFile, "")
	flagSet.BoolVar(&cfg.Utilization, "utilization", cfg.Utilization, "")
	flagSet.BoolVar(&cfg.Foreground, "f", cfg.Foreground, "")
	flagSet.BoolVar(&cfg.Foreground, "foreground", cfg.Foreground, "")
	flagSet.BoolVar(&cfg.Agent, "agent", cfg.Agent, "")
	flagSet.StringVar(&cfg.CAFile, "cafile", cfg.CAFile, "")
	flagSet.StringVar(&cfg.CAPath, "capath", cfg.CAPath, "")
	flagSet.BoolVar(&cfg.IntegrationMode, "integration", cfg.IntegrationMode, "")
	flagSet.IntVar(&cfg.PProfPort, "pprof", cfg.PProfPort, "")
	flagSet.BoolVar(&printVersion, "version", false, "")
	flagSet.BoolVar(&printVersion, "v", false, "")
	flagSet.Var(config.NewFlagParserShim(cfg), "define", "")

	return flagSet
}

func createLegacyFlagSet(cfg *Config) *flag.FlagSet {
	legacyFlagSet := flag.NewFlagSet("", flag.ContinueOnError)
	legacyFlagSet.SetOutput(ioutil.Discard)
	legacyFlagSet.Usage = func() { fmt.Fprint(os.Stderr, "") }

	legacyFlagSet.StringVar(&cfg.ConfigFile, "c", cfg.ConfigFile, "config file location")
	legacyFlagSet.StringVar(&cfg.BindAddr, "P", cfg.BindAddr, "")
	legacyFlagSet.StringVar(&cfg.Proxy, "x", cfg.Proxy, "")
	legacyFlagSet.StringVar(&cfg.Pidfile, "p", cfg.Pidfile, "")
	// no-pidfile needs to be in here to pass to the worker process
	legacyFlagSet.BoolVar(&cfg.NoPidfile, "no-pidfile", cfg.NoPidfile, "")
	legacyFlagSet.StringVar(&cfg.LogFile, "l", cfg.LogFile, "")
	legacyFlagSet.Var(&cfg.LogLevel, "d", "LogLevel")
	legacyFlagSet.StringVar(&cfg.AuditFile, "a", cfg.AuditFile, "")
	legacyFlagSet.BoolVar(&cfg.Foreground, "f", cfg.Foreground, "")
	legacyFlagSet.BoolVar(&cfg.Agent, "A", cfg.Agent, "")
	legacyFlagSet.StringVar(&cfg.CAFile, "b", cfg.CAFile, "")
	legacyFlagSet.StringVar(&cfg.CAPath, "S", cfg.CAPath, "")

	return legacyFlagSet
}

func parseConfigFile(cfg *Config) error {
	if cfg.ConfigFile != "" {
		if err := config.ParseFile(cfg.ConfigFile, cfg); err != nil {
			return err
		}
	}
	return nil
}

var (
	defaultCfg = Config{
		BindAddr:     newrelic.DefaultListenSocket,
		LogLevel:     log.LogInfo,
		LogFile:      "",
		AuditFile:    "",
		MaxFiles:     2048, // to match the legacy daemon behavior
		NoPidfile:    false,
		DetectAWS:    true,
		DetectAzure:  true,
		DetectGCP:    true,
		DetectPCF:    true,
		DetectDocker: true,
		AppTimeout:   config.Timeout(newrelic.DefaultAppTimeout),
	}
)

// configure parses the command line options, and, if provided, the
// configuration file and returns a Config with the results.
//
// This function exits on error.
func configure() *Config {
	cfg := defaultCfg

	args := os.Args[1:]
	flagSet := createFlagSet(&cfg)

	if err := flagSet.Parse(args); err != nil {
		// If the error was due to the -h or -help flag, print usage.
		if err == flag.ErrHelp {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}

		// Were they trying to use valid legacy flags? We'll reset the config
		// struct in case it was modified and store the error in case the flags
		// turn out to be invalid.
		cfg = defaultCfg
		legacyFlagSet := createLegacyFlagSet(&cfg)
		firstError := err

		if err := legacyFlagSet.Parse(args); err != nil {
			fmt.Fprintf(os.Stderr, "%s\n", firstError)
			fmt.Fprint(os.Stderr, "\n", usage)
			os.Exit(1)
		}

		// We now know they're using valid legacy flags, so warn 'em
		fmt.Fprintf(os.Stderr, legacyNotice)

		if err := parseConfigFile(&cfg); err != nil {
			fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
			os.Exit(1)
		}

		legacyFlagSet.Parse(args)
	} else {
		if err := parseConfigFile(&cfg); err != nil {
			fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
			os.Exit(1)
		}

		// Parse the flags a second time so that command line arguments
		// take precedence over config file values.
		flagSet.Parse(args)
	}

	if cfg.Foreground {
		cfg.Role = RoleWorker
	} else {
		cfg.Role = getRole()
	}

  if cfg.BindPort != "" {
		fmt.Fprint(os.Stderr, "--port is deprecated, use --addr instead\n")
    _, err := strconv.Atoi(cfg.BindPort)
    if err == nil {
      cfg.BindAddr = "127.0.0.1:" + cfg.BindPort
    } else {
      cfg.BindAddr = cfg.BindPort
    }
  }

	return &cfg
}

const RoleEnvironmentVariable = "NEW_RELIC_DAEMON_ROLE"

func getRole() Role {
	switch strings.ToLower(os.Getenv(RoleEnvironmentVariable)) {
	case "watcher":
		return RoleWatcher
	case "worker":
		return RoleWorker
	default:
		return RoleProgenitor
	}
}

func setExitStatus(code int) {
	exitMutex.Lock()
	defer exitMutex.Unlock()

	if code > exitStatus {
		exitStatus = code
	}
}

// spawnWatcher respawns the current process in a new session.
func spawnWatcher(cfg *Config) (*exec.Cmd, error) {
	// The current directory is going to be changed to the root directory
	// just before calling exec(2). If name is relative to the current
	// directory, convert to an absolute path so exec(2) can still find us.
	name, err := exec.LookPath(os.Args[0])
	if err != nil {
		return nil, err
	}

	name, err = filepath.Abs(name)
	if err != nil {
		return nil, err
	}

	env := Environment(os.Environ())
	env.Set(RoleEnvironmentVariable, "watcher")
	env.Set("PWD", "/")

	cmd := exec.Command(name, os.Args[1:]...)
	cmd.Dir = "/"
	cmd.Env = []string(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	return cmd, cmd.Start()
}

// Environment adds convenience methods to []string for treating its
// elements as environment variables in KEY=VALUE form as provided by
// os.Environ() and given to exec.Cmd.Env.
type Environment []string

// Index returns the index of the first instance of key in the Environment.
func (env Environment) Index(key string) int {
	for i := range env {
		eq := strings.IndexByte(env[i], '=')
		if eq != -1 && env[i][:eq] == key {
			return i
		}
	}
	return -1
}

// Set sets the value of the environment variable given by key. The
// environment variable will be added if it does not already exist.
func (env *Environment) Set(key, value string) {
	if i := env.Index(key); i != -1 {
		(*env)[i] = key + "=" + value
	} else {
		*env = append(*env, key+"="+value)
	}
}

// initLog opens the daemon log based on the current configuration settings.
// If no log has been specified, initLog will try the following standard
// locations.
//
//   /var/log/newrelic/newrelic-daemon.log
//   /var/log/newrelic-daemon.log
//
// If no suitable location can be found, a generic error is returned.
func initLog(cfg *Config) error {
	if cfg.LogFile != "" {
		return log.Init(cfg.LogLevel, cfg.LogFile)
	}

	standardDirs := []string{"/var/log/newrelic", "/var/log"}

	for _, dir := range standardDirs {
		path := filepath.Join(dir, "newrelic-daemon.log")
		err := log.Init(cfg.LogLevel, path)
		if err == nil {
			return nil
		}
	}

	return fmt.Errorf("unable to find a suitable log file location, "+
		"please check that %s exists and is writable", standardDirs[0])
}

func (r Role) String() string {
	switch r {
	case RoleProgenitor:
		return "progenitor"
	case RoleWatcher:
		return "watcher"
	case RoleWorker:
		return "worker"
	default:
		return "unknown(" + strconv.Itoa(int(r)) + ")"
	}
}

func banner(cfg *Config) string {
	buf := &bytes.Buffer{}
	fmt.Fprintf(buf, "New Relic daemon version %s [", version.Full())
  fmt.Fprintf(buf, "listen=%q", cfg.BindAddr)

	if cfg.Agent {
		fmt.Fprint(buf, " startup=agent")
	} else {
		fmt.Fprint(buf, " startup=init")
	}

	// process info
	fmt.Fprint(buf, " pid=", os.Getpid())
	fmt.Fprint(buf, " ppid=", os.Getppid())
	fmt.Fprint(buf, " uid=", os.Getuid())
	fmt.Fprint(buf, " euid=", os.Geteuid())
	fmt.Fprint(buf, " gid=", os.Getgid())
	fmt.Fprint(buf, " egid=", os.Getegid())

	// go compiler + runtime info
	fmt.Fprint(buf, ` runtime="`, runtime.Version(), `"`)
	fmt.Fprint(buf, " GOMAXPROCS=", runtime.GOMAXPROCS(-1))
	fmt.Fprint(buf, " GOOS=", runtime.GOOS)
	fmt.Fprint(buf, " GOARCH=", runtime.GOARCH)
	fmt.Fprint(buf, "]")

	return buf.String()
}

// A borkedSyscallError describes the failed handling of missing system calls
// on very old Xen Linux kernels.
type borkedSyscallError string

func (e borkedSyscallError) Error() string {
	version := "unknown"
	if runtime.GOOS == "linux" {
		if v, err := ioutil.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
			version = string(v)
		}
	}

	return fmt.Sprintf(
		"Your operating system is not supported by this New Relic"+
			" product. If you have any questions or believe you reached this"+
			" message in error, please file a ticket with New Relic support."+
			" version=%s reason='%s is missing, but did not return -ENOSYS'",
		version, string(e))
}
