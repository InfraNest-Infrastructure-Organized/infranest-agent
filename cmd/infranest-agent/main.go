// Command infranest-agent collects a handful of numbers from the machine it runs on and posts them to an
// InfraNest account.
//
// It only ever sends. It takes no instructions, executes nothing, and opens no ports: the HTTP response is
// ignored beyond its status code, there is no listening socket, and nothing here runs a subprocess. CI
// checks the first two by inspecting the dependency graph rather than trusting this comment.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	showVersion := fs.Bool("version", false, "print version information and exit")
	withProcesses := fs.Bool("processes", false, "include the top processes by memory")
	withArgs := fs.Bool("process-args", false, "include full command lines — these often contain credentials")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Fprintf(stdout, "infranest-agent %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}

	switch fs.Arg(0) {
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
	default:
		return fmt.Errorf("unknown command %q — run without arguments for usage", fs.Arg(0))
	}
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
  status      what is collecting, and when the last push succeeded
  flare       a redacted bundle for a support ticket
  uninstall   remove the unit, user, binary, config and state

Flags:
  --version        print version information and exit
  --processes      include the top processes by memory
  --process-args   include full command lines — these often contain credentials,
                   which is why the executable name alone is the default

Only 'print' is implemented so far. See the README for what is coming.
`)
}
