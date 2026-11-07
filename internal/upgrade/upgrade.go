/*
Package upgrade is `infranest-agent upgrade` and `infranest-agent verify`: the agent already installed
checks what is about to replace it (InfraNest #5969).

`curl … | sudo sh -s -- --upgrade` trusts whoever serves the script: it runs as root, so the download host
is in effect root on every server that upgrades. The binary it fetches is checked against a checksum from
that same host, and against the release's build attestation only where gh or cosign happens to be
installed — which is almost nowhere.

So the trust starts from what is already on the machine instead:

  - `infranest-agent verify` checks a file against a release's attestation, offline, with the verifier
    and Sigstore trust anchors compiled into this binary (internal/provenance). The installers call the
    *installed* agent's `verify` on every upgrade, so the new binary is checked by the old one, never by
    itself.
  - `infranest-agent upgrade` fetches the release's installer and its attestation, checks the installer
    was built by release.yml for that tag, and prints the one command that runs it. That installer then
    has this agent check the binary it downloads. Every byte that runs as root has been checked by the
    agent that was already here, and the download host can only withhold an upgrade, not replace one.

It does not run the installer itself: the agent starts no processes, and an upgrade is a privileged change
somebody should be able to see before it happens. It is never part of the service: CI fails the build if
the service's dependency graph reaches this package.
*/
package upgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/provenance"
)

const (
	repo = "InfraNest-Infrastructure-Organized/infranest-agent"
	// Attestation is the release asset that holds the build attestation for every file in the release.
	Attestation = "infranest-agent.sigstore.json"
	// The installer, and the largest one we would believe. install.sh is ~40 KB.
	installer      = "install.sh"
	maxInstallSize = 1 << 20
	maxBundleSize  = 1 << 20
)

// Sources are where releases are downloaded from, in the order the installer tries them: our pass-through
// first (GitHub has no IPv6), GitHub second. Both serve /releases/download/<tag>/<file> from one release.
// What they serve is checked, so neither is trusted.
var Sources = []string{"https://get.infranest.io", "https://github.com/" + repo}

var tagPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)`)

// VerifyOptions is `infranest-agent verify`.
type VerifyOptions struct {
	File, Bundle string
	// Name is the file's name in the release (infranest-agent_linux_amd64, install.sh, …). Defaults to
	// File's base name.
	Name string
	// Tag, when set, is the release it must be from.
	Tag string
	// NoDowngrade refuses a release older than Self.
	NoDowngrade bool
	// Self is this binary's own version.
	Self string
}

// Verify checks a file against a release attestation and says who built it.
func Verify(w io.Writer, o VerifyOptions) error {
	if o.File == "" || o.Bundle == "" {
		return errors.New("verify needs --bundle and a file: infranest-agent verify --bundle infranest-agent.sigstore.json FILE")
	}
	if o.Name == "" {
		o.Name = filepath.Base(o.File)
	}
	bundle, err := readCapped(o.Bundle, maxBundleSize)
	if err != nil {
		return err
	}
	f, err := os.Open(o.File)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))

	policy := provenance.Release
	if o.Tag != "" {
		policy = provenance.ForTag(o.Tag)
	}
	res, err := provenance.Verify(bundle, policy, o.Name, digest)
	if err != nil {
		return fmt.Errorf("%s does not verify: %w", o.Name, err)
	}
	if o.NoDowngrade && older(res.Tag, o.Self) {
		return fmt.Errorf("%s is from %s, older than this agent (%s). Not downgrading: ask for that version explicitly to move back on purpose", o.Name, res.Tag, o.Self)
	}

	fmt.Fprintf(w, "%s was built by %s from %s (commit %s), logged in Rekor at index %d on %s\n",
		o.Name, policy.Workflow, res.Tag, res.Commit, res.LogIndex, res.SignedAt.Format(time.RFC3339))
	return nil
}

// Options is `infranest-agent upgrade`.
type Options struct {
	// Version is the release to move to; empty means the latest.
	Version string
	// Self is this binary's own version.
	Self string
	// Dir is where the checked installer is left; empty means a fresh temporary directory.
	Dir string
	// Sources, Latest and Client are overridable for tests.
	Sources []string
	Latest  func(ctx context.Context) (string, error)
	Client  *http.Client
	GOOS    string
	// check is provenance.Verify; tests replace it, because no fixture can hold an installer that a real
	// release attested before the first release that attests one.
	check func([]byte, provenance.Policy, string, [32]byte) (*provenance.Result, error)
}

// Run fetches and checks the release's installer and prints the command that runs it.
func Run(ctx context.Context, w io.Writer, o Options) error {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.GOOS != "linux" {
		return fmt.Errorf("upgrade is for Linux. On Windows run install.ps1 -Upgrade: it has this agent check the new binary the same way")
	}
	if o.Client == nil {
		o.Client = newClient()
	}
	if o.Sources == nil {
		o.Sources = Sources
	}
	if o.check == nil {
		o.check = provenance.Verify
	}
	if o.Latest == nil {
		o.Latest = func(ctx context.Context) (string, error) { return latest(ctx, o.Client) }
	}

	tag := o.Version
	if tag == "" {
		var err error
		if tag, err = o.Latest(ctx); err != nil {
			return err
		}
		// Whoever answers "which is the latest" can only point at a genuine release — but an old genuine
		// release with a known bug is still worth refusing, unless somebody asks for it by name.
		if older(tag, o.Self) {
			return fmt.Errorf("the download host says the latest release is %s, which is older than this agent (%s). Not downgrading; to move back on purpose, pass --version %s", tag, o.Self, tag)
		}
		if tag == o.Self {
			fmt.Fprintf(w, "This agent is already %s, the latest release. Nothing to do.\n", tag)
			return nil
		}
	}
	if !tagPattern.MatchString(tag) {
		return fmt.Errorf("%q is not a release version (vX.Y.Z)", tag)
	}

	fmt.Fprintf(w, "Fetching the installer for %s and its build attestation.\n", tag)
	script, err := fetch(ctx, o.Client, o.Sources, tag, installer, maxInstallSize)
	if err != nil {
		return err
	}
	bundle, err := fetch(ctx, o.Client, o.Sources, tag, Attestation, maxBundleSize)
	if err != nil {
		return fmt.Errorf("%w — without the attestation nothing can say who built this release, so it is not used", err)
	}

	res, err := o.check(bundle, provenance.ForTag(tag), installer, sha256.Sum256(script))
	if err != nil {
		return fmt.Errorf("the installer for %s does not verify, so it is not used: %w", tag, err)
	}

	dir := o.Dir
	if dir == "" {
		// Private to whoever ran this (0700), so nobody else on the machine can swap the file between
		// this check and the moment it is run.
		if dir, err = os.MkdirTemp("", "infranest-agent-upgrade-"); err != nil {
			return err
		}
	}
	path := filepath.Join(dir, installer)
	if err := os.WriteFile(path, script, 0o600); err != nil {
		return err
	}

	fmt.Fprintf(w, `
Checked: %s for %s was built by %s
  from commit %s, logged in Rekor at index %d on %s.

To upgrade, run it:

    sudo sh %s --upgrade --version %s

It downloads the new binary and has this agent (%s) check that binary's attestation the same way
before it replaces anything. It keeps agent.conf as it is and restarts the service.
`, installer, tag, provenance.Release.Workflow, res.Commit, res.LogIndex, res.SignedAt.Format(time.RFC3339),
		path, tag, o.Self)
	return nil
}

// latest asks which release is the newest, the same two ways the installer does.
func latest(ctx context.Context, c *http.Client) (string, error) {
	get := func(url string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		return c.Do(req)
	}
	if res, err := get("https://get.infranest.io/releases/latest"); err == nil {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		res.Body.Close()
		if tag := strings.TrimSpace(string(body)); res.StatusCode == http.StatusOK && tagPattern.MatchString(tag) {
			return tag, nil
		}
	}
	// GitHub answers with a redirect to …/releases/tag/<tag>; the tag is read from it, not followed.
	noFollow := *c
	noFollow.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://github.com/"+repo+"/releases/latest", nil)
	if err == nil {
		if res, err := noFollow.Do(req); err == nil {
			res.Body.Close()
			_, tag, _ := strings.Cut(res.Header.Get("Location"), "/releases/tag/")
			if tagPattern.MatchString(tag) {
				return tag, nil
			}
		}
	}
	return "", errors.New("could not find the latest release — neither get.infranest.io nor github.com answered")
}

// fetch downloads one release asset from the first source that serves it.
func fetch(ctx context.Context, c *http.Client, sources []string, tag, file string, limit int64) ([]byte, error) {
	var errs []string
	for _, src := range sources {
		b, err := fetchOne(ctx, c, src+"/releases/download/"+tag+"/"+file, limit)
		if err == nil {
			return b, nil
		}
		errs = append(errs, err.Error())
	}
	return nil, fmt.Errorf("could not download %s for %s: %s", file, tag, strings.Join(errs, "; "))
}

func fetchOne(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", req.URL.Host, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s sent more than any genuine file would be", req.URL.Host)
	}
	return b, nil
}

// newClient is bounded like every other network call here, and follows only HTTPS redirects: GitHub sends
// a release download to its asset host, and nothing should ever send one to plain HTTP.
func newClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", req.URL.Scheme)
			}
			return nil
		},
	}
}

// older reports whether release tag a is older than b. A version that is not a release tag — a build
// from source says "dev" — is never older or newer than anything.
func older(a, b string) bool {
	pa, oka := parse(a)
	pb, okb := parse(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func parse(tag string) ([3]int, bool) {
	m := tagPattern.FindStringSubmatch(tag)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than any attestation we publish", path)
	}
	return b, nil
}
