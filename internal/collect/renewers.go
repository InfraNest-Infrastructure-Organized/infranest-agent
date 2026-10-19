package collect

import (
	"bufio"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Renewer is one certificate something on this machine renews (#3149) — what renews it, which names, and,
// where systemd keeps it, when that renewer last ran and how it went.
//
// Read only from places an unprivileged user can read. That is the agent's public promise, and it shapes
// what can be known: certbot's renewal configs are world-readable and say which certificates it renews,
// while its private keys, and on most installs the certificates themselves, are not. Caddy and Traefik keep
// their storage owner-only — Traefik refuses to start otherwise — so on a stock install all that can be
// said about them is that the storage is there and refused us. That is reported as such (`Unreadable`)
// rather than skipped: a store we could not read is a different fact from no store.
type Renewer struct {
	// certbot, caddy or traefik.
	Tool string `json:"tool"`
	// certbot's lineage (the renewal config's name), or the certificate's main name for Caddy and Traefik.
	Name string `json:"name,omitempty"`
	// The names it renews. From the certificate when it could be read; for certbot otherwise from the
	// webroot map, and failing that the lineage name — then `DomainsGuessed` says so.
	Domains        []string `json:"domains,omitempty"`
	DomainsGuessed bool     `json:"domains_guessed,omitempty"`
	// The certificate's expiry, when its file could be read.
	NotAfter *time.Time `json:"not_after,omitempty"`
	// The file this was read from — or, with `Unreadable`, the one that refused us.
	Source string `json:"source,omitempty"`
	// certbot's authenticator: webroot, nginx, apache, standalone, dns-cloudflare…
	Authenticator string `json:"authenticator,omitempty"`
	// certbot's installer — nginx or apache — which reloads that web server itself after a renewal. Absent
	// where there is none, which is the common case for webroot, standalone and DNS authenticators.
	Installer string `json:"installer,omitempty"`
	// What else certbot runs after renewing this certificate (InfraNest #4066): `deploy` (a deploy hook — the
	// lineage's `renew_hook`, an executable in `renewal-hooks/deploy/`, or `deploy-hook` in `cli.ini`) and
	// `post` (the same three places for post hooks). Only which kinds, never the commands, which can carry
	// secrets. `[]` means every place was read and none is set: a renewal then changes the files and nothing
	// tells the web server. Absent where a place could not be read, which is not "none".
	Hooks *[]string `json:"hooks,omitempty"`

	// The timer (or service) that runs it, and what systemd recorded about its last run: when the timer
	// last fired, how the service last finished (`success`, or systemd's own failure word), and when it
	// fires next. Absent where there is no such unit — certbot from cron, Caddy renewing in-process.
	Unit       string     `json:"unit,omitempty"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	LastResult string     `json:"last_result,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`

	// The store exists and refused us. Mode and ownership say whether that is the distribution's design
	// (owner-only, root) or somebody's permissions.
	Unreadable bool   `json:"unreadable,omitempty"`
	Mode       string `json:"mode,omitempty"`
	RootOwned  bool   `json:"root_owned,omitempty"`
}

const (
	maxRenewers       = 50
	maxRenewerDomains = 100
	maxDomain         = 253
	maxRenewerSource  = 512
	maxAuthenticator  = 64
	// A renewal config is a few hundred bytes and a certificate a few kilobytes; Traefik's store holds every
	// certificate it manages. The cap keeps a replaced or corrupted file from being read whole.
	maxRenewerFileBytes = 4 << 20
)

// Where each tool keeps what it renews, on the layouts its packages install.
const certbotRenewalDir = "/etc/letsencrypt/renewal"

// Where certbot looks for hooks besides a lineage's own config: a directory per kind, whose executables it
// runs for every renewal, and the global defaults file.
const (
	certbotHooksDir = "/etc/letsencrypt/renewal-hooks"
	certbotCliIni   = "/etc/letsencrypt/cli.ini"
)

var caddyStores = []string{
	"/var/lib/caddy/.local/share/caddy/certificates",
	"/var/lib/caddy/certificates",
}

var traefikStores = []string{
	"/etc/traefik/acme.json",
	"/etc/traefik/acme/acme.json",
	"/etc/traefik/letsencrypt/acme.json",
	"/letsencrypt/acme.json",
	"/opt/traefik/acme.json",
}

// readRenewers looks for every renewer under root ("/" outside tests). Never fails: a tool that is not
// installed contributes nothing, and one whose files refused us contributes an Unreadable entry.
func readRenewers(root string) []Renewer {
	out := readCertbot(root)
	out = append(out, readCaddy(root)...)
	out = append(out, readTraefik(root)...)

	if len(out) > maxRenewers {
		out = out[:maxRenewers]
	}

	return out
}

func readCertbot(root string) []Renewer {
	dir := filepath.Join(root, certbotRenewalDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []Renewer{unreadable("certbot", dir, certbotRenewalDir)}
	}

	global, globalKnown := readCertbotGlobalHooks(root)

	var out []Renewer
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		shown := filepath.Join(certbotRenewalDir, e.Name())

		f, err := os.Open(path)
		if err != nil {
			out = append(out, unreadable("certbot", path, shown))

			continue
		}
		r := parseCertbotRenewal(strings.TrimSuffix(e.Name(), ".conf"), io.LimitReader(f, maxRenewerFileBytes))
		_ = f.Close()
		r.Source = clip(shown, maxRenewerSource)
		r.Hooks = mergeHooks(r.hooks, global, globalKnown)

		// The certificate itself, when this user may read it — on most installs `live/` is root-only, and
		// the names then come from the config. Read through root so tests can point it at a fixture.
		if r.certPath != "" {
			if names, notAfter, ok := readCertFile(filepath.Join(root, r.certPath)); ok {
				r.Domains, r.DomainsGuessed, r.NotAfter = names, false, &notAfter
			}
		}
		out = append(out, r.Renewer)
	}

	return out
}

type certbotRenewal struct {
	Renewer
	certPath string
	// The hook kinds the lineage's own config sets.
	hooks map[string]bool
}

// A lineage certbot had to disambiguate: `example.com-0001` is a second certificate for example.com.
var lineageSuffix = regexp.MustCompile(`-\d{4}$`)

// parseCertbotRenewal reads one `renewal/<lineage>.conf`. The format is configobj: `key = value` at the
// top, `[renewalparams]`, and `[[webroot_map]]` with one `domain = path` line per name. Split from the file
// read so captured configs can be tested anywhere.
func parseCertbotRenewal(lineage string, r io.Reader) certbotRenewal {
	out := certbotRenewal{Renewer: Renewer{Tool: "certbot", Name: clip(lineage, 255)}, hooks: map[string]bool{}}
	section := ""
	var webroot []string

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[] ")

			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		switch {
		case section == "" && key == "cert":
			out.certPath = value
		case section == "renewalparams" && key == "authenticator":
			out.Authenticator = clip(value, maxAuthenticator)
		case section == "renewalparams" && key == "installer" && !isNone(value):
			out.Installer = clip(value, maxAuthenticator)
		// certbot stores a deploy hook as `renew_hook`, its old name.
		case section == "renewalparams" && (key == "renew_hook" || key == "deploy_hook") && !isNone(value):
			out.hooks["deploy"] = true
		case section == "renewalparams" && key == "post_hook" && !isNone(value):
			out.hooks["post"] = true
		case section == "webroot_map":
			webroot = append(webroot, key)
		}
	}

	if len(webroot) > 0 {
		out.Domains = cleanDomains(webroot)
	} else if name := lineageSuffix.ReplaceAllString(lineage, ""); name != "" {
		out.Domains, out.DomainsGuessed = cleanDomains([]string{name}), true
	}

	return out
}

// isNone is configobj's empty value: certbot writes `None` for an option that was not set.
func isNone(value string) bool {
	return value == "" || value == "None"
}

// readCertbotGlobalHooks reads the hook kinds certbot runs for every lineage: an executable file in
// `renewal-hooks/deploy/` or `post/`, or `deploy-hook`/`renew-hook`/`post-hook` in `cli.ini`. A place that
// does not exist sets nothing; one that exists and refused us makes the answer unknown (`known` false).
func readCertbotGlobalHooks(root string) (kinds map[string]bool, known bool) {
	kinds = map[string]bool{}
	known = true
	for _, kind := range []string{"deploy", "post"} {
		entries, err := os.ReadDir(filepath.Join(root, certbotHooksDir, kind))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			known = false

			continue
		}
		for _, e := range entries {
			info, err := e.Info()
			// certbot runs only executable regular files; a README or a disabled script is not a hook.
			if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
				kinds[kind] = true
			}
		}
	}

	f, err := os.Open(filepath.Join(root, certbotCliIni))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		known = false
	default:
		for k := range parseCertbotCliIni(io.LimitReader(f, maxRenewerFileBytes)) {
			kinds[k] = true
		}
		_ = f.Close()
	}

	return kinds, known
}

// parseCertbotCliIni reads the hook kinds `cli.ini` sets: `key = value` (or a bare key) per line, the
// command-line option names without their dashes.
func parseCertbotCliIni(r io.Reader) map[string]bool {
	kinds := map[string]bool{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if isNone(value) {
			continue
		}
		switch strings.ReplaceAll(key, "_", "-") {
		case "deploy-hook", "renew-hook":
			kinds["deploy"] = true
		case "post-hook":
			kinds["post"] = true
		}
	}

	return kinds
}

// mergeHooks is the lineage's hook kinds plus the global ones, sorted. Unknown (nil) when a global place
// could not be read and the lineage sets none itself — one it does set is known whatever the rest says.
func mergeHooks(lineage, global map[string]bool, globalKnown bool) *[]string {
	all := map[string]bool{}
	for k := range lineage {
		all[k] = true
	}
	for k := range global {
		all[k] = true
	}
	if !globalKnown && len(all) == 0 {
		return nil
	}
	out := make([]string, 0, len(all))
	for k := range all {
		out = append(out, k)
	}
	sort.Strings(out)

	return &out
}

// readCaddy reads Caddy's certificate metadata: `<store>/<issuer>/<name>/<name>.json` and the `.crt` beside
// it. Owner-only on a stock install, which makes the usual answer one Unreadable entry for the store.
func readCaddy(root string) []Renewer {
	var out []Renewer
	for _, store := range caddyStores {
		dir := filepath.Join(root, store)
		issuers, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			out = append(out, unreadable("caddy", dir, store))

			continue
		}
		for _, issuer := range issuers {
			if !issuer.IsDir() {
				continue
			}
			names, err := os.ReadDir(filepath.Join(dir, issuer.Name()))
			if err != nil {
				out = append(out, unreadable("caddy", filepath.Join(dir, issuer.Name()), filepath.Join(store, issuer.Name())))

				continue
			}
			for _, n := range names {
				if !n.IsDir() {
					continue
				}
				out = append(out, readCaddyCert(root, store, issuer.Name(), n.Name()))
			}
		}
	}

	return out
}

func readCaddyCert(root, store, issuer, name string) Renewer {
	rel := filepath.Join(store, issuer, name, name)
	r := Renewer{Tool: "caddy", Name: clip(name, 255), Source: clip(rel+".crt", maxRenewerSource)}

	if names, notAfter, ok := readCertFile(filepath.Join(root, rel+".crt")); ok {
		r.Domains, r.NotAfter = names, &notAfter

		return r
	}

	if f, err := os.Open(filepath.Join(root, rel+".json")); err == nil {
		var meta struct {
			SANs []string `json:"sans"`
		}
		if json.NewDecoder(io.LimitReader(f, maxRenewerFileBytes)).Decode(&meta) == nil && len(meta.SANs) > 0 {
			r.Domains = cleanDomains(meta.SANs)
			r.Source = clip(rel+".json", maxRenewerSource)
		}
		_ = f.Close()
		if len(r.Domains) > 0 {
			return r
		}
	}

	return unreadable("caddy", filepath.Join(root, rel+".crt"), rel+".crt")
}

// readTraefik reads Traefik's ACME store. Traefik insists on mode 0600, so unless the agent runs as the
// same user this is always an Unreadable entry — which is still worth sending: it says Traefik renews here.
func readTraefik(root string) []Renewer {
	var out []Renewer
	for _, store := range traefikStores {
		path := filepath.Join(root, store)
		f, err := os.Open(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			out = append(out, unreadable("traefik", path, store))

			continue
		}
		certs := parseTraefikStore(io.LimitReader(f, maxRenewerFileBytes))
		_ = f.Close()
		for i := range certs {
			certs[i].Source = clip(store, maxRenewerSource)
		}
		out = append(out, certs...)
	}

	return out
}

// parseTraefikStore reads `acme.json`: one object per certificate resolver, each with its certificates
// as base64 PEM. Only the public certificate is decoded; the key beside it is never touched.
func parseTraefikStore(r io.Reader) []Renewer {
	var store map[string]struct {
		Certificates []struct {
			Domain struct {
				Main string   `json:"main"`
				SANs []string `json:"sans"`
			} `json:"domain"`
			Certificate string `json:"certificate"`
		} `json:"Certificates"`
	}
	if json.NewDecoder(r).Decode(&store) != nil {
		return nil
	}

	resolvers := make([]string, 0, len(store))
	for name := range store {
		resolvers = append(resolvers, name)
	}
	sort.Strings(resolvers)

	var out []Renewer
	for _, resolver := range resolvers {
		for _, c := range store[resolver].Certificates {
			rn := Renewer{Tool: "traefik", Name: clip(c.Domain.Main, 255), Domains: cleanDomains(append([]string{c.Domain.Main}, c.Domain.SANs...))}
			if raw, err := base64.StdEncoding.DecodeString(c.Certificate); err == nil {
				if names, notAfter, ok := parseCertPEM(raw); ok {
					rn.Domains, rn.NotAfter = names, &notAfter
				}
			}
			out = append(out, rn)
		}
	}

	return out
}

// readCertFile reads the first certificate of a PEM file: its names and its expiry.
func readCertFile(path string) ([]string, time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	defer f.Close()

	raw, err := io.ReadAll(io.LimitReader(f, maxRenewerFileBytes))
	if err != nil {
		return nil, time.Time{}, false
	}

	return parseCertPEM(raw)
}

func parseCertPEM(raw []byte) ([]string, time.Time, bool) {
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, time.Time{}, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, time.Time{}, false
	}
	names := cert.DNSNames
	if len(names) == 0 && cert.Subject.CommonName != "" {
		names = []string{cert.Subject.CommonName}
	}

	return cleanDomains(names), cert.NotAfter.UTC(), true
}

// unreadable names a store that refused us — `shown` is the path as it is on the machine, without the
// test root.
func unreadable(tool, path, shown string) Renewer {
	r := Renewer{Tool: tool, Source: clip(shown, maxRenewerSource), Unreadable: true}
	if info, err := os.Lstat(path); err == nil {
		r.Mode = fmt.Sprintf("%#o", info.Mode().Perm())
		r.RootOwned = isRootOwned(info)
	}

	return r
}

// cleanDomains lower-cases, drops empties and duplicates, clips each name and caps the list.
func cleanDomains(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] || len(d) > maxDomain {
			continue
		}
		seen[d] = true
		out = append(out, d)
		if len(out) == maxRenewerDomains {
			break
		}
	}

	return out
}

// certbotRun is what systemd recorded about certbot's timer and service.
type certbotRun struct {
	unit   string
	last   *time.Time
	next   *time.Time
	result string
}

// applyCertbotRun puts one run on every certbot certificate: `certbot renew` renews them all in one run,
// so its outcome is theirs. Split from the bus read so it can be tested without one.
func applyCertbotRun(renewers []Renewer, run certbotRun) {
	for i := range renewers {
		if renewers[i].Tool != "certbot" || renewers[i].Unreadable {
			continue
		}
		renewers[i].Unit = run.unit
		renewers[i].LastRunAt = run.last
		renewers[i].NextRunAt = run.next
		renewers[i].LastResult = run.result
	}
}

func hasReadableTool(renewers []Renewer, tool string) bool {
	for _, r := range renewers {
		if r.Tool == tool && !r.Unreadable {
			return true
		}
	}

	return false
}
