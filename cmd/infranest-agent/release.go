package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/upgrade"
)

// verifyCommand is `infranest-agent verify` (InfraNest #5969). Its own flags, because they mean nothing to
// any other command, and the installers call it with exactly these.
func verifyCommand(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("infranest-agent verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := upgrade.VerifyOptions{Self: version}
	fs.StringVar(&o.Bundle, "bundle", "", "the release's attestation (infranest-agent.sigstore.json)")
	fs.StringVar(&o.Name, "name", "", "the file's name in the release (default: its own name)")
	fs.StringVar(&o.Tag, "tag", "", "the release it must be from, e.g. v0.24.0 (default: any release)")
	fs.BoolVar(&o.NoDowngrade, "no-downgrade", false, "refuse a release older than this agent")
	fs.Usage = func() {
		fmt.Fprint(stdout, `infranest-agent verify --bundle FILE [--name NAME] [--tag vX.Y.Z] [--no-downgrade] FILE

Checks that FILE was built by this repository's release workflow, from its build attestation. Offline:
the verifier and Sigstore's trust anchors are part of this binary, so it needs no gh, no cosign and no
network. Exits non-zero, saying why, for anything it does not accept.

`)
		fs.SetOutput(stdout)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("verify checks one file: infranest-agent verify --bundle infranest-agent.sigstore.json FILE")
	}
	o.File = fs.Arg(0)

	return upgrade.Verify(stdout, o)
}

// upgradeCommand is `infranest-agent upgrade`.
func upgradeCommand(args []string, stdout, stderr *os.File) error {
	fs := flag.NewFlagSet("infranest-agent upgrade", flag.ContinueOnError)
	fs.SetOutput(stderr)
	o := upgrade.Options{Self: version}
	fs.StringVar(&o.Version, "version", "", "the release to move to (default: the latest)")
	fs.Usage = func() {
		fmt.Fprint(stdout, `infranest-agent upgrade [--version vX.Y.Z]

Fetches the release's installer, checks with this agent that it was built by the release workflow, and
prints the command that runs it. That installer has this agent check the new binary the same way before
replacing anything. Refuses a release older than this agent unless it is asked for by name.

`)
		fs.SetOutput(stdout)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return upgrade.Run(ctx, stdout, o)
}
