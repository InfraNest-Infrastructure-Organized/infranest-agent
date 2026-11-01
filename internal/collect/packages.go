package collect

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Package is the installed package a reported process's executable belongs to (InfraNest #5665): what turns
// "postgres" into "PostgreSQL 16.4 from the package postgresql-16", and names the long tail the receiver's own
// catalogue does not know by the package's own one-line description.
//
// Read from the package manager's database, which every user may read — `/var/lib/dpkg/status` and
// `/var/lib/dpkg/info/*.list` — and never from running the package manager: the agent executes nothing.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// The package's own one-line summary (dpkg's `Description:` first line), as the packager wrote it.
	Description string `json:"description,omitempty"`
	// Which database it came from: "dpkg". Room for "rpm" without changing the shape.
	Source string `json:"source"`
	// How the process was matched to a file of the package, strongest first:
	//   - "executable": the kernel's own link to the running file, /proc/<pid>/exe. Exact, but readable only
	//     for the agent's own processes, so rare;
	//   - "command": the absolute path the process was started by, the first word of its command line, when
	//     its base name is the kernel's name for the process. A program can rewrite its command line, so this
	//     is what it says it is;
	//   - "name": the kernel's name for the process looked up among the standard program directories, used
	//     only when exactly one package installs a program of that name there.
	//
	// And for a systemd unit (InfraNest #5789), "unit_file": the file systemd loaded the unit from, its
	// `FragmentPath`, is one the package installed. Exact — dpkg lists the file by name.
	Match string `json:"match"`
	// dpkg's `Priority` — "required", "important", "standard", "optional" or "extra" — and `Section` ("admin",
	// "universe/net"), as the packager set them (InfraNest #5789). The first three priorities are what a
	// minimal Debian or Ubuntu system installs, which is what tells the base system's own units from software
	// somebody added. Absent when the package does not say.
	Priority string `json:"priority,omitempty"`
	Section  string `json:"section,omitempty"`
}

const (
	maxPackageName        = 128
	maxPackageVersion     = 128
	maxPackageDescription = 255
	maxPackagePriority    = 32
	maxPackageSection     = 64

	// Bounds on one rebuild of the index. A Debian or Ubuntu server has a few hundred to a few thousand
	// packages and tens of thousands of files; these are far past that, and are here so a corrupt or
	// hostile database cannot make the agent read or hold without limit (the unit allows it 64 MB).
	maxIndexedPackages = 50000
	maxIndexedPaths    = 200000
	maxStatusBytes     = 64 << 20
	maxListBytes       = 8 << 20

	// A package upgrade rewrites dpkg's status file several times in a minute. The index is rebuilt when the
	// file changes, but not more often than this: between two rebuilds a just-upgraded package reads at its
	// previous version, which is better than re-reading every file list four times during one `apt upgrade`.
	minIndexRebuild = 5 * time.Minute
)

// The directories a command is looked up in by name. A name alone is a weak match, so it is tried only in the
// places a shell would find a program, and only when one package owns that name among them.
var programDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

type packageInfo struct {
	name, version, description, priority, section string
}

// The directories systemd loads unit files from that a package may install into (InfraNest #5789), and the two
// an administrator writes their own into. A unit file in /etc/systemd/system that no package owns was written
// on this machine — by hand, or by an installer script — and is the machine's own.
var (
	unitDirs      = []string{"/lib/systemd/system", "/usr/lib/systemd/system", "/etc/systemd/system", "/usr/local/lib/systemd/system"}
	localUnitDirs = []string{"/etc/systemd/system", "/usr/local/lib/systemd/system"}
)

// PackageIndex maps a file path to the dpkg package that installed it. Built from dpkg's database and kept
// between collections; rebuilt only when the status file's modification time moves (and not more often than
// minIndexRebuild), so a steady machine reads one `stat` per collection and nothing else.
//
// Only files under a `bin`, `sbin` or `libexec` directory are indexed — the places executables live — and the
// `.service` and `.timer` files directly in systemd's unit directories (InfraNest #5789), which keeps the index
// to a few thousand entries rather than every file the system has.
type PackageIndex struct {
	statusPath string
	infoDir    string

	mu       sync.Mutex
	mtime    time.Time
	builtAt  time.Time
	built    bool
	byPath   map[string]string // path -> package key ("name" or "name:arch")
	byName   map[string]string // program name in programDirs -> key, "" when two packages have it
	packages map[string]packageInfo
}

// NewPackageIndex returns an index over the system's dpkg database. Nothing is read until it is used, and on a
// machine without dpkg it stays empty and costs one failed `stat` per collection.
func NewPackageIndex() *PackageIndex {
	return newPackageIndexAt("/var/lib/dpkg")
}

func newPackageIndexAt(dir string) *PackageIndex {
	return &PackageIndex{statusPath: filepath.Join(dir, "status"), infoDir: filepath.Join(dir, "info")}
}

// refresh rebuilds the index when dpkg's database changed since the last build. Reports whether an index is
// available afterwards.
func (x *PackageIndex) refresh(now time.Time) bool {
	info, err := os.Stat(x.statusPath)
	if err != nil {
		// No dpkg here (RHEL, Alpine, Windows) or it is unreadable: nothing to say, which is absent, not a
		// failure — most of the fleet runs one package manager or the other.
		return false
	}
	if x.built && (info.ModTime().Equal(x.mtime) || now.Sub(x.builtAt) < minIndexRebuild) {
		return true
	}

	packages, err := readDpkgStatus(x.statusPath)
	if err != nil {
		return x.built
	}
	byPath, byName := readDpkgLists(x.infoDir, packages)

	x.packages, x.byPath, x.byName = packages, byPath, byName
	x.mtime, x.builtAt, x.built = info.ModTime(), now, true

	return true
}

// lookup names the package a path belongs to, trying the path as given and its twin across the /usr merge:
// a merged system runs /usr/bin/bash while the package lists /bin/bash, and the reverse.
func (x *PackageIndex) lookup(path string) (packageInfo, bool) {
	for _, candidate := range usrMergeTwins(filepath.Clean(path)) {
		if key, ok := x.byPath[candidate]; ok {
			if p, ok := x.packages[key]; ok {
				return p, true
			}
		}
	}

	return packageInfo{}, false
}

// lookupName finds the one package that installs a program of this name in a standard program directory. The
// kernel's name for a process is its executable's base name cut to 15 bytes, so a longer name is matched on
// that prefix — and refused when two programs share it.
func (x *PackageIndex) lookupName(comm string) (packageInfo, bool) {
	key, ok := x.byName[comm]
	if !ok || key == "" {
		return packageInfo{}, false
	}
	p, ok := x.packages[key]

	return p, ok
}

func usrMergeTwins(path string) []string {
	for _, top := range []string{"/bin/", "/sbin/", "/lib/", "/lib64/"} {
		if strings.HasPrefix(path, top) {
			return []string{path, "/usr" + path}
		}
		if strings.HasPrefix(path, "/usr"+top) {
			return []string{path, strings.TrimPrefix(path, "/usr")}
		}
	}

	return []string{path}
}

// readDpkgStatus reads the installed packages from dpkg's status file.
func readDpkgStatus(path string) (map[string]packageInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return parseDpkgStatus(io.LimitReader(f, maxStatusBytes)), nil
}

// parseDpkgStatus reads the stanzas of a dpkg status file, keeping the installed packages: their name,
// version and the first line of their description. Keyed by "name:arch" and by "name", because a package's
// file list is named either way — "libc6:amd64.list" for a multi-arch package, "nginx-core.list" otherwise.
func parseDpkgStatus(r io.Reader) map[string]packageInfo {
	out := map[string]packageInfo{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)

	var name, arch, version, description, status, priority, section string
	flush := func() {
		if name != "" && version != "" && strings.HasSuffix(status, " installed") && len(out) < maxIndexedPackages {
			p := packageInfo{
				name:        clip(name, maxPackageName),
				version:     clip(version, maxPackageVersion),
				description: clip(description, maxPackageDescription),
				priority:    clip(priority, maxPackagePriority),
				section:     clip(section, maxPackageSection),
			}
			if arch != "" {
				out[name+":"+arch] = p
			}
			if _, taken := out[name]; !taken {
				out[name] = p
			}
		}
		name, arch, version, description, status, priority, section = "", "", "", "", "", "", ""
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()

			continue
		}
		// A continuation line (the long description, a conffile list) starts with a space.
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Package":
			name = value
		case "Architecture":
			arch = value
		case "Version":
			version = value
		case "Status":
			status = value
		case "Description":
			description = value
		case "Priority":
			priority = value
		case "Section":
			section = value
		}
	}
	flush()

	return out
}

// readDpkgLists reads every package's file list and indexes the executables and unit files among them.
func readDpkgLists(dir string, packages map[string]packageInfo) (byPath, byName map[string]string) {
	byPath = map[string]string{}
	byName = map[string]string{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return byPath, byName
	}

	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".list")
		if !ok || e.IsDir() {
			continue
		}
		key := stem
		if _, known := packages[key]; !known {
			// "name:arch.list" for a package installed for one architecture only has its status stanza keyed by
			// name alone too; a list with no installed package (removed, config files left) is skipped.
			if n, _, cut := strings.Cut(stem, ":"); cut {
				if _, known := packages[n]; known {
					key = n
				} else {
					continue
				}
			} else {
				continue
			}
		}

		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		indexList(io.LimitReader(f, maxListBytes), key, byPath, byName)
		f.Close()

		if len(byPath) >= maxIndexedPaths {
			break
		}
	}

	return byPath, byName
}

// indexList adds one package's executables and unit files to the index. A path claimed by two packages is dropped from the
// name index (ambiguous) and kept by the first for the path index — dpkg itself refuses two owners of one file
// outside diversions, so a clash here is a diversion and the first owner is the one dpkg lists first.
func indexList(r io.Reader, key string, byPath, byName map[string]string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 16<<10), 64<<10)
	for scanner.Scan() {
		path := scanner.Text()
		if !executablePath(path) && !unitFilePath(path) {
			continue
		}
		if len(byPath) >= maxIndexedPaths {
			return
		}
		if _, taken := byPath[path]; !taken {
			byPath[path] = key
		}

		dir, base := filepath.Split(path)
		dir = strings.TrimSuffix(dir, "/")
		for _, d := range programDirs {
			if dir != d {
				continue
			}
			name := clip(base, 15)
			if was, seen := byName[name]; seen && was != key {
				byName[name] = ""
			} else if !seen {
				byName[name] = key
			}
		}
	}
}

// executablePath is whether a listed path sits below a bin, sbin or libexec directory — where programs are
// installed. Directories themselves (".../bin") end in that segment and are not below it.
func executablePath(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	for _, s := range segments[:len(segments)-1] {
		if s == "bin" || s == "sbin" || s == "libexec" {
			return true
		}
	}

	return false
}

// unitFilePath is whether a listed path is a service or timer unit file directly in one of systemd's unit
// directories — the units the agent reports. Drop-ins (`foo.service.d/override.conf`) are not the unit's own
// file and are not indexed.
func unitFilePath(path string) bool {
	if !strings.HasSuffix(path, ".service") && !strings.HasSuffix(path, ".timer") {
		return false
	}
	dir := filepath.Dir(path)
	for _, d := range unitDirs {
		if dir == d {
			return true
		}
	}

	return false
}

// localUnitFile is whether systemd loaded a unit from a directory an administrator writes into.
func localUnitFile(path string) bool {
	dir := filepath.Dir(filepath.Clean(path))
	for _, d := range localUnitDirs {
		if dir == d {
			return true
		}
	}

	return false
}

// fragmentSource answers, for one unit's D-Bus object path, the file systemd loaded the unit from. The system
// bus connection is one; a test passes its own answers.
type fragmentSource interface {
	FragmentPath(unitPath string) (string, error)
}

// readFragments asks for the unit file of each reported service (InfraNest #5789): one property read per unit,
// on the connection the services collector already holds, after the list was cut to what is sent. An error or an
// empty answer leaves it unknown — a unit with no file of its own (a transient unit) has none.
func readFragments(services []Service, src fragmentSource) {
	if src == nil {
		return
	}
	for i := range services {
		if services[i].objectPath == "" {
			continue
		}
		if path, err := src.FragmentPath(services[i].objectPath); err == nil && strings.HasPrefix(path, "/") {
			services[i].fragment = path
		}
	}
}

// attachUnitPackages names the package each reported unit's file belongs to (InfraNest #5789), from the same
// index as the processes' packages, and marks the unit local when its file sits in an administrator's directory
// and no package owns it. Nothing is said about a unit whose file could not be read or placed — absent, never
// guessed. A unit loaded from /run (a generator's output) is neither.
func attachUnitPackages(services []Service, idx *PackageIndex, now time.Time) {
	if len(services) == 0 {
		return
	}
	indexed := false
	if idx != nil {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		indexed = idx.refresh(now)
	}

	for i := range services {
		s := &services[i]
		if s.fragment == "" {
			continue
		}
		if indexed {
			if p, ok := idx.lookup(s.fragment); ok {
				s.Package = p.toPackage("unit_file")

				continue
			}
		}
		if localUnitFile(s.fragment) {
			local := true
			s.Local = &local
		}
	}
}

func (p packageInfo) toPackage(match string) *Package {
	return &Package{
		Name: p.name, Version: p.version, Description: p.description,
		Source: "dpkg", Match: match, Priority: p.priority, Section: p.section,
	}
}

// attachPackages names the package of each reported process (InfraNest #5665), for the reported few only.
//
// A process inside a container is skipped: its files are the container's image, not this machine's packages,
// and a /usr/sbin/nginx in there would otherwise be named after the host's nginx. So is anything that cannot be
// placed on a file of an installed package — absent, never guessed.
//
// The command line is read here only for its first word, in memory, to find the file a process was started
// from; it is not sent, whether or not command lines are switched on.
func attachPackages(procs []Process, idx *PackageIndex, procRoot string, now time.Time) {
	if idx == nil || len(procs) == 0 {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if !idx.refresh(now) {
		return
	}

	for i := range procs {
		p := &procs[i]
		if p.ContainerID != "" || p.PID <= 0 {
			continue
		}
		dir := filepath.Join(procRoot, strconv.Itoa(p.PID))

		statBytes, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		comm, err := parseProcComm(string(statBytes))
		if err != nil || comm == "" {
			continue
		}

		if pkg, match, ok := idx.resolve(dir, comm); ok {
			p.Package = pkg.toPackage(match)
		}
	}
}

// resolve finds a process's package from the strongest evidence that can be read: the running file, the path it
// was started by, its name in a program directory.
func (x *PackageIndex) resolve(dir, comm string) (packageInfo, string, bool) {
	if exe, err := os.Readlink(filepath.Join(dir, "exe")); err == nil {
		// A file replaced since the process started — the usual state between an upgrade and a restart — is
		// "(deleted)". The package now holds a different version from the one running, so nothing is said.
		if strings.HasSuffix(exe, " (deleted)") {
			return packageInfo{}, "", false
		}
		if p, ok := x.lookup(exe); ok {
			return p, "executable", true
		}
	}

	if argv0 := firstArg(filepath.Join(dir, "cmdline")); strings.HasPrefix(argv0, "/") && clip(filepath.Base(argv0), 15) == comm {
		if p, ok := x.lookup(argv0); ok {
			return p, "command", true
		}
	}

	if p, ok := x.lookupName(comm); ok {
		return p, "name", true
	}

	return packageInfo{}, "", false
}

// firstArg reads the first NUL-terminated word of a command line, bounded.
func firstArg(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	first, _, _ := strings.Cut(string(buf[:n]), "\x00")

	return first
}
