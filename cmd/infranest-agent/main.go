// Command infranest-agent collects a handful of numbers from the machine it runs on and posts them to an
// InfraNest account.
//
// It only ever sends. It takes no instructions, executes nothing, and opens no ports: the HTTP response is
// ignored beyond its status code, there is no listening socket, and nothing here runs a subprocess. CI
// checks the first two by inspecting the dependency graph rather than trusting this comment.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/collect"
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
		})
	case "run", "status", "flare", "uninstall":
		// Recognised, so the error says what is actually true rather than "unknown command", which would
		// send someone hunting for a typo that is not there. The installers already reference `run` and
		// `status`; naming them here keeps the story consistent while the sending half is written.
		return fmt.Errorf("%q is not implemented yet — this build can only 'print'", command)
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

func usage(w *os.File) {
	fmt.Fprint(w, `infranest-agent — the InfraNest monitoring agent

It only sends. It takes no instructions, executes nothing, and opens no ports.

Usage:
  infranest-agent <command>

Commands:
  print       run one collection cycle and write what would be sent to stdout, sending nothing
  run         collect continuously and send                      (not implemented yet)
  status      what is collecting, and when the last push succeeded (not implemented yet)
  flare       a redacted bundle for a support ticket              (not implemented yet)
  uninstall   remove the unit, user, binary, config and state     (not implemented yet)

Flags:
  --version        print version information and exit
  --processes      include the top processes by memory
  --process-args   include full command lines — these often contain credentials,
                   which is why the executable name alone is the default

Only 'print' is implemented so far. See the README for what is coming.
`)
}
