// Command infranest-agent collects a handful of numbers from the machine it runs on and posts them to an
// InfraNest account.
//
// It only ever sends. It takes no instructions, executes nothing, and opens no ports: the HTTP response is
// ignored beyond its status code, there is no listening socket, and nothing here runs a subprocess. CI
// checks the first two by inspecting the dependency graph rather than trusting this comment.
package main

import (
	"flag"
	"fmt"
	"os"
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
	default:
		return fmt.Errorf("unknown command %q — run without arguments for usage", fs.Arg(0))
	}
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
  --version   print version information and exit

Not implemented yet — this is the scaffold. See the README for what is coming.
`)
}
