package collect

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Setup is how the machine itself is set up (InfraNest #5815, contract 24): who may log in over SSH and how, who
// is an administrator, and whether security updates install by themselves. The settings behind most break-ins and
// most neglected servers, and every one of them written in a file any user can read.
//
// Read, never asked: no `sshd -T` (it needs root and it executes), no `getent`, no package manager. What a file the
// agent may not read would have decided is reported as unknown — never as the program's default, because "passwords
// are off" read from a file that was not read is the one claim somebody would act on by doing nothing.
type Setup struct {
	// The SSH server's effective global settings. Absent where no /etc/ssh/sshd_config exists (no OpenSSH server).
	SSH *SSHSetup `json:"ssh,omitempty"`
	// The administrator groups that exist here — sudo, wheel and admin — with their members. Absent when /etc/group
	// could not be read; a group the machine does not have is not listed.
	AdminGroups *[]AdminGroup `json:"admin_groups,omitempty"`
	// How security updates are installed. Absent on a system that is neither Debian-like nor RHEL-like.
	Updates *UpdatesSetup `json:"updates,omitempty"`
}

// SSHSetup is sshd's global configuration as sshd itself would resolve it: /etc/ssh/sshd_config and the files it
// includes, in order, the first value of each keyword winning — which is why the drop-ins Debian and Ubuntu include
// at the top of the file override what is written below it.
type SSHSetup struct {
	// "yes", "no", or "unknown" when a file that could have set it first could not be read.
	PasswordAuthentication string `json:"password_authentication"`
	// KbdInteractiveAuthentication, or its older name ChallengeResponseAuthentication — one setting. With UsePAM it is
	// the other way a password is asked for.
	KbdInteractiveAuthentication string `json:"kbd_interactive_authentication"`
	UsePAM                       string `json:"use_pam"`
	// The required methods, as written ("publickey", "publickey,password"), "any" when not set, or "unknown".
	AuthenticationMethods string `json:"authentication_methods"`
	// "yes", "prohibit-password" (or its old name "without-password"), "forced-commands-only", "no", or "unknown".
	PermitRootLogin string `json:"permit_root_login"`
	// Every Port sshd listens on. Absent when unknown: any file that could not be read may add one.
	Ports []int `json:"ports,omitempty"`
	// Where each setting was decided, by its keyword in lower case: the file and line. A keyword not listed takes
	// sshd's built-in default.
	SetIn map[string]string `json:"set_in,omitempty"`
	// The keywords a Match block sets — for some users or addresses they differ from the values above.
	Match []string `json:"match,omitempty"`
	// Files and folders sshd reads that the agent could not.
	Unreadable []string `json:"unreadable,omitempty"`
}

// AdminGroup is a group whose members may run anything as root through sudo (or su, for wheel).
type AdminGroup struct {
	Group string `json:"group"`
	// Sorted user names: the group's member list in /etc/group, and the users whose primary group it is.
	Members []string `json:"members"`
}

// UpdatesSetup is where the automatic-update setting is decided, so "it is off" can say in which file.
// `system.auto_update` (contract 10) still says whether it runs; this is the evidence beside it.
type UpdatesSetup struct {
	// "debian" (apt, unattended-upgrades) or "rhel" (dnf, dnf-automatic).
	Family string `json:"family"`
	// "unattended-upgrades" or "dnf-automatic".
	Tool string `json:"tool"`
	// Whether the tool is installed: dpkg's status database on Debian, the tool's own config file on RHEL. Absent when
	// it could not be told.
	Installed *bool `json:"installed,omitempty"`
	// APT::Periodic::Unattended-Upgrade as apt resolves it (the last value wins), or dnf-automatic's apply_updates.
	// Absent when nothing sets it.
	Setting string `json:"setting,omitempty"`
	// The file that set it.
	SetIn string `json:"set_in,omitempty"`
	// APT::Periodic::Enable when something sets it — "0" switches every periodic task off at once.
	PeriodicEnable string `json:"periodic_enable,omitempty"`
	// RHEL: which dnf-automatic timer is enabled — "install", "plain", or absent for none.
	Timer string `json:"timer,omitempty"`
}

// Bounds. sshd itself stops including at a depth of 16.
const (
	maxSSHDepth      = 16
	maxSSHFiles      = 100
	maxSSHPorts      = 16
	maxSetupFileSize = 1 << 20
	maxSetupValue    = 128
	maxSetupPath     = 255
	maxGroupMembers  = 64
	maxDpkgStatus    = 64 << 20
)

// The administrator groups, in the order they are reported.
var adminGroupNames = []string{"sudo", "wheel", "admin"}

// The SSH keywords read, and sshd's built-in defaults for them (OpenSSH 8.7 and later).
var sshKeywords = map[string]string{
	"passwordauthentication":       "yes",
	"kbdinteractiveauthentication": "yes",
	"usepam":                       "no",
	"authenticationmethods":        "any",
	"permitrootlogin":              "prohibit-password",
}

// SetupAvailable is whether this platform has a setup collector: Linux. Windows keeps these settings in the registry
// and in policy the agent does not read, and says nothing rather than something wrong.
func SetupAvailable() bool { return runtime.GOOS == "linux" }

// SetupReader reads the setup and keeps the answer until one of the files it read changes. Held by the run loop for
// the life of the process: these files change a few times a year, and dpkg's status file is megabytes.
type SetupReader struct {
	mu          sync.Mutex
	fingerprint string
	touched     []string
	setup       *Setup
}

// NewSetupReader is a reader with nothing cached.
func NewSetupReader() *SetupReader {
	return &SetupReader{}
}

// Read answers from root ("/" outside tests). Nil when there is nothing to report.
func (r *SetupReader) Read(root string) *Setup {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.touched != nil && fingerprintOf(root, r.touched) == r.fingerprint {
		return r.setup
	}

	t := &touchList{}
	r.setup = readSetup(root, t)
	r.touched = t.paths
	r.fingerprint = fingerprintOf(root, r.touched)

	return r.setup
}

// touchList is every file and folder one read looked at — present or not, so a file that appears is noticed too.
type touchList struct {
	paths []string
	seen  map[string]bool
}

func (t *touchList) add(p string) {
	if t.seen == nil {
		t.seen = map[string]bool{}
	}
	if !t.seen[p] {
		t.seen[p] = true
		t.paths = append(t.paths, p)
	}
}

func fingerprintOf(root string, paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		if fi, err := os.Stat(filepath.Join(root, p)); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d:%o;", p, fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
		} else {
			fmt.Fprintf(&b, "%s:-;", p)
		}
	}

	return b.String()
}

func readSetup(root string, t *touchList) *Setup {
	s := Setup{
		SSH:         readSSH(root, t),
		AdminGroups: readAdminGroups(root, t),
		Updates:     readUpdates(root, t),
	}
	if s.SSH == nil && s.AdminGroups == nil && s.Updates == nil {
		return nil
	}

	return &s
}

// ── SSH ──────────────────────────────────────────────────────────────────────────────────────────────────────────

type sshState struct {
	root       string
	t          *touchList
	values     map[string]string
	setIn      map[string]string
	ports      []int
	portsSet   bool
	match      map[string]bool
	unreadable []string
	// A file that could have decided something could not be read: whatever was not decided before it is unknown.
	blocked bool
	files   int
}

func readSSH(root string, t *touchList) *SSHSetup {
	const main = "/etc/ssh/sshd_config"
	t.add(main)
	if _, err := os.Stat(filepath.Join(root, main)); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	st := &sshState{root: root, t: t, values: map[string]string{}, setIn: map[string]string{}, match: map[string]bool{}}
	st.file(main, false, 0)

	out := SSHSetup{SetIn: map[string]string{}}
	resolve := func(keyword string) string {
		if v, ok := st.values[keyword]; ok {
			out.SetIn[keyword] = st.setIn[keyword]

			return v
		}
		if st.blocked {
			return "unknown"
		}

		return sshKeywords[keyword]
	}
	out.PasswordAuthentication = resolve("passwordauthentication")
	out.KbdInteractiveAuthentication = resolve("kbdinteractiveauthentication")
	out.UsePAM = resolve("usepam")
	out.AuthenticationMethods = resolve("authenticationmethods")
	out.PermitRootLogin = resolve("permitrootlogin")

	switch {
	case st.blocked:
		// Port accumulates: any unread file may add one.
	case st.portsSet:
		out.Ports = st.ports
		out.SetIn["port"] = st.setIn["port"]
	default:
		out.Ports = []int{22}
	}

	for k := range st.match {
		out.Match = append(out.Match, k)
	}
	sort.Strings(out.Match)
	out.Unreadable = st.unreadable
	if len(out.SetIn) == 0 {
		out.SetIn = nil
	}

	return &out
}

// file reads one configuration file. inMatch is whether the line that included it sits inside a Match block: a
// Match inside the file holds until the end of that file, and the including file's state is untouched by it.
func (st *sshState) file(p string, inMatch bool, depth int) {
	st.files++
	if st.files > maxSSHFiles {
		return
	}
	st.t.add(p)
	f, err := os.Open(filepath.Join(st.root, p))
	if err != nil {
		st.cannotRead(p, inMatch)

		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(io.LimitReader(f, maxSetupFileSize))
	scanner.Buffer(make([]byte, 0, 64<<10), maxSetupFileSize)
	line := 0
	for scanner.Scan() {
		line++
		keyword, args := sshLine(scanner.Text())
		if keyword == "" {
			continue
		}
		switch keyword {
		case "match":
			inMatch = true
		case "include":
			if depth+1 > maxSSHDepth {
				continue
			}
			for _, arg := range args {
				for _, inc := range st.expand(arg, inMatch) {
					st.file(inc, inMatch, depth+1)
				}
			}
		case "port":
			if len(args) == 0 {
				continue
			}
			if inMatch {
				continue // Port is not allowed in a Match block; sshd refuses the file
			}
			n, err := strconv.Atoi(args[0])
			if err != nil || n < 1 || n > 65535 {
				continue
			}
			if !st.portsSet {
				st.setIn["port"] = clip(fmt.Sprintf("%s:%d", p, line), maxSetupPath)
			}
			st.portsSet = true
			if len(st.ports) < maxSSHPorts && !containsInt(st.ports, n) {
				st.ports = append(st.ports, n)
			}
		default:
			if keyword == "challengeresponseauthentication" {
				keyword = "kbdinteractiveauthentication"
			}
			if _, tracked := sshKeywords[keyword]; !tracked || len(args) == 0 {
				continue
			}
			if inMatch {
				st.match[keyword] = true

				continue
			}
			if _, done := st.values[keyword]; done || st.blocked {
				continue // first value wins; after an unread file, a later value may not be the first
			}
			value := strings.ToLower(args[0])
			if keyword == "authenticationmethods" {
				value = strings.Join(args, " ")
			}
			st.values[keyword] = clip(value, maxSetupValue)
			st.setIn[keyword] = clip(fmt.Sprintf("%s:%d", p, line), maxSetupPath)
		}
	}
	if scanner.Err() != nil {
		st.cannotRead(p, inMatch)
	}
}

func (st *sshState) cannotRead(p string, inMatch bool) {
	if len(st.unreadable) < maxSSHFiles {
		st.unreadable = append(st.unreadable, clip(p, maxSetupPath))
	}
	if !inMatch {
		st.blocked = true
	}
}

// expand resolves one Include argument as sshd does: relative to /etc/ssh, glob patterns matched in lexical order,
// a pattern matching nothing skipped. A folder the agent cannot list may hold files sshd reads.
func (st *sshState) expand(arg string, inMatch bool) []string {
	if !strings.HasPrefix(arg, "/") {
		arg = "/etc/ssh/" + arg
	}
	arg = path.Clean(arg)
	dir := path.Dir(arg)
	st.t.add(dir)

	if !strings.ContainsAny(arg, "*?[") {
		if _, err := os.Lstat(filepath.Join(st.root, arg)); errors.Is(err, fs.ErrNotExist) {
			if _, derr := os.ReadDir(filepath.Join(st.root, dir)); derr != nil && !errors.Is(derr, fs.ErrNotExist) {
				st.cannotRead(dir, inMatch)
			}

			return nil
		}

		return []string{arg}
	}

	entries, err := os.ReadDir(filepath.Join(st.root, dir))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			st.cannotRead(dir, inMatch)
		}

		return nil
	}
	pattern := path.Base(arg)
	var out []string
	for _, e := range entries {
		if ok, _ := path.Match(pattern, e.Name()); ok && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, dir+"/"+e.Name())
		}
	}
	sort.Strings(out)

	return out
}

// sshLine splits a configuration line into its keyword (lower case) and arguments: `Keyword value`,
// `Keyword=value` and `Keyword = value` alike, double quotes grouping an argument, a word starting with `#` starting
// a comment.
func sshLine(line string) (string, []string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", nil
	}

	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		return strings.ToLower(line), nil
	}
	keyword := strings.ToLower(line[:end])
	rest := strings.TrimLeft(line[end:], " \t")
	if strings.HasPrefix(rest, "=") {
		rest = rest[1:]
	}

	var args []string
	var cur strings.Builder
	quoted, have := false, false
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '"':
			quoted = !quoted
			have = true
		case !quoted && (c == ' ' || c == '\t'):
			if have {
				args = append(args, cur.String())
				cur.Reset()
				have = false
			}
		case !quoted && c == '#' && !have:
			i = len(rest)
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	if have {
		args = append(args, cur.String())
	}

	return keyword, args
}

func containsInt(list []int, n int) bool {
	for _, v := range list {
		if v == n {
			return true
		}
	}

	return false
}

// ── Administrator groups ─────────────────────────────────────────────────────────────────────────────────────────

func readAdminGroups(root string, t *touchList) *[]AdminGroup {
	t.add("/etc/group")
	t.add("/etc/passwd")
	f, err := os.Open(filepath.Join(root, "etc/group"))
	if err != nil {
		return nil
	}
	defer f.Close()

	members := map[string]map[string]bool{}
	gids := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(f, maxSetupFileSize))
	scanner.Buffer(make([]byte, 0, 64<<10), maxSetupFileSize)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if len(parts) < 4 || !isAdminGroup(parts[0]) {
			continue
		}
		if _, dup := members[parts[0]]; dup {
			continue // the first entry is the one the C library returns
		}
		set := map[string]bool{}
		for _, m := range strings.Split(parts[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				set[m] = true
			}
		}
		members[parts[0]] = set
		gids[parts[2]] = parts[0]
	}
	if scanner.Err() != nil {
		return nil
	}

	// Users whose primary group it is are members too, without being listed in /etc/group.
	if pf, err := os.Open(filepath.Join(root, "etc/passwd")); err == nil {
		ps := bufio.NewScanner(io.LimitReader(pf, maxSetupFileSize))
		ps.Buffer(make([]byte, 0, 64<<10), maxSetupFileSize)
		for ps.Scan() {
			parts := strings.Split(ps.Text(), ":")
			if len(parts) < 4 || parts[0] == "" || strings.HasPrefix(parts[0], "#") {
				continue
			}
			if group, ok := gids[parts[3]]; ok {
				members[group][parts[0]] = true
			}
		}
		_ = pf.Close()
	}

	out := []AdminGroup{}
	for _, name := range adminGroupNames {
		set, ok := members[name]
		if !ok {
			continue
		}
		list := make([]string, 0, len(set))
		for m := range set {
			list = append(list, clip(m, 64))
		}
		sort.Strings(list)
		if len(list) > maxGroupMembers {
			list = list[:maxGroupMembers]
		}
		out = append(out, AdminGroup{Group: name, Members: list})
	}

	return &out
}

func isAdminGroup(name string) bool {
	for _, g := range adminGroupNames {
		if g == name {
			return true
		}
	}

	return false
}

// ── Automatic updates ────────────────────────────────────────────────────────────────────────────────────────────

func readUpdates(root string, t *touchList) *UpdatesSetup {
	t.add("/etc/debian_version")
	t.add("/var/lib/dpkg/status")
	t.add("/etc/redhat-release")
	switch {
	case exists(filepath.Join(root, "etc/debian_version")) || exists(filepath.Join(root, "var/lib/dpkg/status")):
		return aptUpdates(root, t)
	case exists(filepath.Join(root, "etc/redhat-release")):
		return dnfUpdates(root, t)
	}

	return nil
}

// aptUpdates finds where APT::Periodic::Unattended-Upgrade is decided: apt.conf.d in name order, then apt.conf, the
// last assignment winning — and asks dpkg's own database whether unattended-upgrades is installed.
func aptUpdates(root string, t *touchList) *UpdatesSetup {
	u := UpdatesSetup{Family: "debian", Tool: "unattended-upgrades"}

	const dir = "/etc/apt/apt.conf.d"
	t.add(dir)
	entries, _ := os.ReadDir(filepath.Join(root, dir))
	var files []string
	for _, e := range entries {
		if !e.IsDir() && aptPartName(e.Name()) {
			files = append(files, dir+"/"+e.Name())
		}
	}
	sort.Strings(files)
	if len(files) > maxAptFiles {
		files = files[:maxAptFiles]
	}
	for _, p := range append(files, "/etc/apt/apt.conf") {
		t.add(p)
		f, err := os.Open(filepath.Join(root, p))
		if err != nil {
			continue
		}
		conf := map[string]string{}
		parseAptConf(io.LimitReader(f, maxConfigBytes), conf)
		_ = f.Close()
		if v, ok := conf["apt::periodic::unattended-upgrade"]; ok {
			u.Setting, u.SetIn = clip(v, maxSetupValue), p
		}
		if v, ok := conf["apt::periodic::enable"]; ok {
			u.PeriodicEnable = clip(v, maxSetupValue)
		}
	}

	if installed, ok := dpkgInstalled(filepath.Join(root, "var/lib/dpkg/status"), "unattended-upgrades"); ok {
		u.Installed = &installed
	}

	return &u
}

// dpkgInstalled reads dpkg's status database for one package: installed when its Status ends "installed". ok is false
// when the database could not be read.
func dpkgInstalled(statusFile, pkg string) (installed bool, ok bool) {
	f, err := os.Open(statusFile)
	if err != nil {
		return false, false
	}
	defer f.Close()

	scanner := bufio.NewScanner(io.LimitReader(f, maxDpkgStatus))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	current := ""
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			current = ""
		case strings.HasPrefix(line, "Package: "):
			current = strings.TrimSpace(strings.TrimPrefix(line, "Package: "))
		case current == pkg && strings.HasPrefix(line, "Status: "):
			return strings.HasSuffix(strings.TrimSpace(line), " installed"), true
		}
	}
	if scanner.Err() != nil {
		return false, false
	}

	return false, true
}

// dnfUpdates reads dnf-automatic's config and which of its timers is enabled. Installed is the config file being
// there: the RPM database is not a format the agent reads.
func dnfUpdates(root string, t *touchList) *UpdatesSetup {
	u := UpdatesSetup{Family: "rhel", Tool: "dnf-automatic"}
	installed := false
	for _, p := range []string{"/etc/dnf/automatic.conf", "/etc/dnf/dnf5-plugins/automatic.conf"} {
		t.add(p)
		f, err := os.Open(filepath.Join(root, p))
		if err != nil {
			continue
		}
		installed = true
		ini := parseINI(io.LimitReader(f, maxConfigBytes))
		_ = f.Close()
		if v, ok := ini["commands.apply_updates"]; ok {
			u.Setting, u.SetIn = clip(v, maxSetupValue), p
		}

		break
	}
	u.Installed = &installed

	for _, dir := range []string{"/etc/systemd/system/timers.target.wants", "/usr/lib/systemd/system/timers.target.wants"} {
		t.add(dir)
		has := func(name string) bool { return exists(filepath.Join(root, dir, name)) }
		switch {
		case has("dnf-automatic-install.timer"), has("dnf5-automatic-install.timer"):
			u.Timer = "install"
		case has("dnf-automatic.timer"), has("dnf5-automatic.timer"):
			u.Timer = "plain"
		}
		if u.Timer != "" {
			break
		}
	}

	return &u
}
