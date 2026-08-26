// Command infranest-agent collects a handful of numbers from the machine it runs on and posts them to an
// InfraNest account.
//
// It only ever sends. It takes no instructions, executes nothing, and opens no ports: the HTTP response is
// read only to be reported on, there is no listening socket, and nothing here runs a subprocess. CI
// checks the first two by inspecting the dependency graph rather than trusting this comment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/agent"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/collect"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/config"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/push"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/spool"
)

// Set by the build. Reported by --version and sent with each push, so a fleet running a version with a
// known bug is visible rather than something to be discovered.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "infranest-agent:", err)
		os.Exit(1)
	}
}

// run is separated from main so tests can drive it with their own arguments and capture its output, rather
// than reaching for a subprocess — which the agent is not allowed to spawn in the first place.
func run(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("infranest-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { usage(stdout) }
	showVersion := fs.Bool("version", false, "print version information and exit")
	withProcesses := fs.Bool("processes", false, "include the top processes by memory")
	withArgs := fs.Bool("process-args", false, "include full command lines — these often contain credentials")
	// Explicit, so neither `run` nor `status` depends on a service manager having set an environment
	// variable. A Windows scheduled task inherits nothing, and the Linux unit's EnvironmentFile is one
	// override away from pointing somewhere else.
	configPath := fs.String("config", "", "path to the agent configuration file")

	// Go's flag package stops parsing at the first non-flag argument, so `print --processes` would leave
	// the flag unparsed and silently false — the command would succeed, print no processes, and give
	// nobody a reason why. Pulling the subcommand out first means flags work on either side of it, which
	// is what anyone typing it will expect.
	command, rest := splitCommand(args)

	if err := fs.Parse(rest); err != nil {
		// `--help` is someone asking a question, not getting something wrong: answer it on stdout and
		// exit 0. flag.ErrHelp arrives here after fs.Usage has already printed.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	// A second positional means a typo or a misunderstanding, and dropping it silently is how
	// `print --processes` came to do nothing. Saying so costs a line.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — only one command at a time", fs.Arg(0))
	}

	if *showVersion {
		fmt.Fprintf(stdout, "infranest-agent %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}

	switch command {
	case "", "help":
		usage(stdout)
		return nil
	case "print":
		return printSample(stdout, collect.Options{
			Processes:    *withProcesses || *withArgs,
			ProcessArgs:  *withArgs,
			MaxProcesses: 10,
			CPUInterval:  300 * time.Millisecond,
			// `print` shows what would be sent, so it shows this too — it is the fastest way for somebody
			// to see which units the agent considers watched on their own machine.
			Services: true,
		})
	case "run":
		return runAgent(stdout, stderr, environment(*configPath))
	case "status":
		return showStatus(stdout, environment(*configPath))
	case "uninstall":
		return showUninstall(stdout, environment(*configPath))
	case "flare":
		// Everything a support conversation needs, with the token removed before anything is printed.
		// The alternative is asking somebody to paste a file containing a live credential into a ticket,
		// and they will, because it is what was asked for.
		return agent.WriteFlare(stdout, environment(*configPath), version, time.Now())
	default:
		return fmt.Errorf("unknown command %q — run without arguments for usage", command)
	}
}

// splitCommand pulls the first non-flag argument out, returning it and everything else.
//
// This is what lets flags appear on either side of the subcommand. The alternative — telling people to
// write `--processes print` — is a rule nobody will remember and nothing enforces.
func splitCommand(args []string) (command string, rest []string) {
	rest = make([]string, 0, len(args))

	for _, arg := range args {
		if command == "" && arg != "" && !strings.HasPrefix(arg, "-") {
			command = arg
			continue
		}
		rest = append(rest, arg)
	}

	return command, rest
}

// printSample runs one collection cycle and writes what would be posted, sending nothing.
//
// This is the first thing the README documents, and deliberately so: it is a better answer to "what does
// this collect?" than any prose, because it is the reader's own machine answering with their own data.
// That means the output has to be exactly the payload — not a summary of it, and not a prettier version.
func printSample(w *os.File, opts collect.Options) error {
	sample, err := collect.Collect(opts)
	if err != nil {
		return err
	}
	sample.AgentVersion = version

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(sample)
}

// runAgent collects and sends until it is told to stop.
//
// Everything it needs comes from the environment, which systemd fills from /etc/infranest/agent.conf and
// Windows from the scheduled task. Configuration errors are fatal *here* rather than warned about and
// carried on with: an agent that starts, reports healthy to the service manager, and sends nowhere is
// indistinguishable — from the only place anyone is looking — from a server that has gone down.
func runAgent(stdout, stderr *os.File, getenv func(string) string) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}

	sp, err := spool.New(cfg.StateDir + "/spool")
	if err != nil {
		return err
	}

	// SIGTERM is how systemd stops a service and how a container is asked to exit. Handling it means the
	// reading in flight finishes and the state file is written, instead of the process being killed
	// mid-write and `status` reporting nonsense on the next start.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stdout, "infranest-agent %s: sending to %s every %s\n", version, cfg.PushURL(), cfg.Interval)

	runner := &agent.Runner{
		Config: cfg,
		Sender: push.New(cfg.Token, version),
		Spool:  sp,
		Log:    stderr,
	}

	return runner.Run(ctx)
}

// environment is the process environment, with --config answering for INFRANEST_CONFIG when given.
//
// The flag wins over the variable: somebody typing a path on the command line is being more specific than
// whatever a service manager did or did not set.
func environment(configPath string) func(string) string {
	if configPath == "" {
		return os.Getenv
	}

	return func(key string) string {
		if key == "INFRANEST_CONFIG" {
			return configPath
		}

		return os.Getenv(key)
	}
}

// showStatus answers "is this working?" from what is on this machine, reaching nothing.
func showStatus(stdout *os.File, getenv func(string) string) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return err
	}

	agent.Status(stdout, cfg, time.Now())

	return nil
}

/*
showUninstall prints the exact commands that remove this installation.

**It does not perform them, and it cannot.** Removing the agent means stopping and disabling a systemd
unit and deleting a system user, and both are subprocesses — `os/exec` is not in this binary and CI fails
the build if it appears. Stopping the unit over D-Bus is the same story from the other direction: that is
`StopUnit`, which polkit refuses to an unprivileged service and which CI also forbids by name.

So #792 asked for "a command, not a docs page", and the honest answer is that it cannot be a command
without giving up the property the whole agent is built around. What it can be is better than a docs page:
the paths below are read from *this* installation rather than assumed, so they are right even where
somebody installed to a different prefix, and they can be pasted without being adapted.

A half-uninstall — deleting the config and state the binary *can* reach, leaving a running service whose
files have vanished — would be worse than either.
*/
func showUninstall(w *os.File, getenv func(string) string) error {
	// Best effort: an uninstall must work when the configuration is broken, which is one of the reasons
	// somebody reaches for it. Where the config cannot be read, the documented defaults are printed.
	cfg, _ := config.Load(getenv)

	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = "/var/lib/infranest-agent"
	}
	confPath := strings.TrimSpace(getenv("INFRANEST_CONFIG"))
	if confPath == "" {
		confPath = config.DefaultConfigPath(getenv)
	}

	binary, err := os.Executable()
	if err != nil || binary == "" {
		binary = "/usr/local/bin/infranest-agent"
	}

	fmt.Fprint(w, `Removing the agent takes two things this binary deliberately cannot do: stop a systemd
unit and delete a system user. Both are subprocesses, and this agent runs none — which is
the property that makes it worth installing. So here are the commands, filled in for
this machine:

`)
	fmt.Fprintln(w, "  sudo systemctl disable --now infranest-agent")
	fmt.Fprintln(w, "  sudo rm -f /etc/systemd/system/infranest-agent.service")
	fmt.Fprintln(w, "  sudo systemctl daemon-reload")
	fmt.Fprintf(w, "  sudo rm -f %s\n", binary)
	fmt.Fprintf(w, "  sudo rm -rf %s %s\n", filepath.Dir(confPath), stateDir)
	fmt.Fprintln(w, "  sudo userdel infranest-agent")
	fmt.Fprint(w, `
Or, if you still have the installer:

  sudo sh install.sh --uninstall

Nothing here phones home, and nothing is left behind once these have run: the token file
and the spool both live in the directories above.
`)

	return nil
}

func usage(w *os.File) {
	fmt.Fprint(w, `infranest-agent — the InfraNest monitoring agent

It only sends. It takes no instructions, executes nothing, and opens no ports.

Usage:
  infranest-agent <command>

Commands:
  print       run one collection cycle and write what would be sent to stdout, sending nothing
  run         collect continuously and send — this is what the service runs
  status      whether readings are being delivered, and if not, why not
  uninstall   print the exact commands that remove this installation
  flare       a redacted bundle for a support ticket — no token in it

Flags:
  --version        print version information and exit
  --processes      include the top processes by memory
  --process-args   include full command lines (Linux only) — these often contain
                   credentials, which is why the executable name alone is the default
  --config <path>  read settings from this file instead of the installed one

Configuration (from the environment; systemd reads /etc/infranest/agent.conf):
  INFRANEST_TOKEN        the server token             (required)
  INFRANEST_URL          where to send                (default https://ingest.infranest.app)
  INFRANEST_INTERVAL     how often to collect         (default 60s, between 10s and 5m)
  INFRANEST_STATE_DIR    spool and state              (default /var/lib/infranest-agent)
  INFRANEST_PROCESSES    collect the busiest processes
  INFRANEST_PROCESS_ARGS include command lines — these often contain credentials
  INFRANEST_SERVICES     watch systemd units and report the failed ones (default on)

A reading that cannot be delivered is kept on disk and sent when the connection comes
back, so a network problem costs nothing but the delay.
`)
}
