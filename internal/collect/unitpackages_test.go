package collect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A trimmed dpkg database from a stock Ubuntu 24.04 cloud image (InfraNest #5789): base-system packages at
// priority required/important/standard, one added at optional, and the unit files each installs — under /lib on
// a package built before the /usr merge, under /usr/lib on one built after.
const unitDpkgStatus = `Package: openssh-server
Status: install ok installed
Priority: optional
Section: net
Architecture: amd64
Version: 1:9.6p1-3ubuntu13.5
Description: secure shell (SSH) server, for secure access from remote machines

Package: systemd
Status: install ok installed
Priority: important
Section: admin
Architecture: amd64
Version: 255.4-1ubuntu8.4
Description: system and service manager

Package: networkd-dispatcher
Status: install ok installed
Priority: optional
Section: universe/utils
Architecture: all
Version: 2.2.4-1
Description: Dispatcher service for systemd-networkd connection status changes

Package: uuid-runtime
Status: install ok installed
Priority: standard
Section: utils
Architecture: amd64
Version: 2.39.3-9ubuntu6.1
Description: runtime components for the Universally Unique ID library

Package: ghosttool
Status: deinstall ok config-files
Priority: optional
Architecture: amd64
Version: 1.0
Description: removed, its unit file list left behind
`

func writeUnitDpkg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("status", unitDpkgStatus)
	write("info/openssh-server.list", "/lib/systemd/system\n/lib/systemd/system/ssh.service\n/lib/systemd/system/ssh.socket\n/usr/sbin/sshd\n")
	write("info/systemd.list", "/usr/lib/systemd/system/systemd-journald.service\n/usr/lib/systemd/system/systemd-tmpfiles-clean.timer\n"+
		"/usr/lib/systemd/system/getty@.service\n/usr/lib/systemd/system/user@.service.d/10-login-barrier.conf\n/usr/lib/systemd/system-generators/systemd-fstab-generator\n")
	write("info/networkd-dispatcher.list", "/lib/systemd/system/networkd-dispatcher.service\n/usr/bin/networkd-dispatcher\n")
	write("info/uuid-runtime.list", "/usr/lib/systemd/system/uuidd.service\n/usr/lib/systemd/system/uuidd.socket\n/usr/sbin/uuidd\n")
	write("info/ghosttool.list", "/lib/systemd/system/ghosttool.service\n")

	return dir
}

// fakeBus answers FragmentPath the way systemd does over D-Bus: by unit object path, with an error for an object
// it does not know.
type fakeBus map[string]string

func (b fakeBus) FragmentPath(unitPath string) (string, error) {
	path, ok := b[unitPath]
	if !ok {
		return "", errors.New("org.freedesktop.DBus.Error.UnknownObject")
	}

	return path, nil
}

func TestAUnitIsNamedByThePackageThatInstalledItsFile(t *testing.T) {
	idx := newPackageIndexAt(writeUnitDpkg(t))

	services := []Service{
		{Unit: "ssh.service", objectPath: "/org/freedesktop/systemd1/unit/ssh_2eservice"},
		// Loaded from /usr/lib on a merged system although the package lists /lib.
		{Unit: "networkd-dispatcher.service", objectPath: "/u/nd"},
		{Unit: "systemd-tmpfiles-clean.timer", objectPath: "/u/tc"},
		// An instance: its file is the template's.
		{Unit: "getty@tty1.service", objectPath: "/u/getty"},
		{Unit: "uuidd.service", objectPath: "/u/uuidd"},
		// Written by an administrator.
		{Unit: "myapp.service", objectPath: "/u/myapp"},
		{Unit: "backup.timer", objectPath: "/u/backup"},
		// Installed by `make install`: in a package directory, but no package owns it — nothing is said.
		{Unit: "vendor-agent.service", objectPath: "/u/vendor"},
		// A generator's output, a transient unit, a unit the bus does not answer for, and the file list of a
		// package that is no longer installed.
		{Unit: "rc-local.service", objectPath: "/u/rc"},
		{Unit: "run-u1.service", objectPath: "/u/transient"},
		{Unit: "vanished.service", objectPath: "/u/gone"},
		{Unit: "ghosttool.service", objectPath: "/u/ghost"},
	}
	readFragments(services, fakeBus{
		"/org/freedesktop/systemd1/unit/ssh_2eservice": "/lib/systemd/system/ssh.service",
		"/u/nd":        "/usr/lib/systemd/system/networkd-dispatcher.service",
		"/u/tc":        "/usr/lib/systemd/system/systemd-tmpfiles-clean.timer",
		"/u/getty":     "/usr/lib/systemd/system/getty@.service",
		"/u/uuidd":     "/usr/lib/systemd/system/uuidd.service",
		"/u/myapp":     "/etc/systemd/system/myapp.service",
		"/u/backup":    "/usr/local/lib/systemd/system/backup.timer",
		"/u/vendor":    "/usr/lib/systemd/system/vendor-agent.service",
		"/u/rc":        "/run/systemd/generator/rc-local.service",
		"/u/transient": "",
		"/u/ghost":     "/lib/systemd/system/ghosttool.service",
	})
	attachUnitPackages(services, idx, time.Now())

	by := map[string]Service{}
	for _, s := range services {
		by[s.Unit] = s
	}

	want := map[string]Package{
		"ssh.service":                  {Name: "openssh-server", Version: "1:9.6p1-3ubuntu13.5", Description: "secure shell (SSH) server, for secure access from remote machines", Source: "dpkg", Match: "unit_file", Priority: "optional", Section: "net"},
		"networkd-dispatcher.service":  {Name: "networkd-dispatcher", Version: "2.2.4-1", Description: "Dispatcher service for systemd-networkd connection status changes", Source: "dpkg", Match: "unit_file", Priority: "optional", Section: "universe/utils"},
		"systemd-tmpfiles-clean.timer": {Name: "systemd", Version: "255.4-1ubuntu8.4", Description: "system and service manager", Source: "dpkg", Match: "unit_file", Priority: "important", Section: "admin"},
		"getty@tty1.service":           {Name: "systemd", Version: "255.4-1ubuntu8.4", Description: "system and service manager", Source: "dpkg", Match: "unit_file", Priority: "important", Section: "admin"},
		"uuidd.service":                {Name: "uuid-runtime", Version: "2.39.3-9ubuntu6.1", Description: "runtime components for the Universally Unique ID library", Source: "dpkg", Match: "unit_file", Priority: "standard", Section: "utils"},
	}
	for unit, w := range want {
		got := by[unit]
		if got.Package == nil {
			t.Fatalf("%s: no package, want %s", unit, w.Name)
		}
		if *got.Package != w {
			t.Errorf("%s: package %+v, want %+v", unit, *got.Package, w)
		}
		if got.Local != nil {
			t.Errorf("%s: a packaged unit is marked local", unit)
		}
	}

	for _, unit := range []string{"myapp.service", "backup.timer"} {
		if s := by[unit]; s.Package != nil || s.Local == nil || !*s.Local {
			t.Errorf("%s: want local with no package, got package=%v local=%v", unit, s.Package, s.Local)
		}
	}
	for _, unit := range []string{"vendor-agent.service", "rc-local.service", "run-u1.service", "vanished.service", "ghosttool.service"} {
		if s := by[unit]; s.Package != nil || s.Local != nil {
			t.Errorf("%s: nothing is known, but package=%v local=%v was said", unit, s.Package, s.Local)
		}
	}

	// Neither the object path nor the unit file path is sent.
	b, _ := json.Marshal(by["ssh.service"])
	if strings.Contains(string(b), "/lib/systemd") || strings.Contains(string(b), "freedesktop") {
		t.Fatalf("the unit's paths leaked into the payload: %s", b)
	}
	if !strings.Contains(string(b), `"match":"unit_file"`) || !strings.Contains(string(b), `"priority":"optional"`) {
		t.Fatalf("the package is not on the wire as documented: %s", b)
	}
}

// A drop-in is not the unit's own file, and a generator's binary is not a unit: neither is indexed.
func TestOnlyUnitFilesInSystemdsUnitDirectoriesAreIndexed(t *testing.T) {
	for path, want := range map[string]bool{
		"/lib/systemd/system/ssh.service":                           true,
		"/usr/lib/systemd/system/fstrim.timer":                      true,
		"/etc/systemd/system/myapp.service":                         true,
		"/usr/lib/systemd/system/ssh.socket":                        false,
		"/usr/lib/systemd/system/user@.service.d/10-barrier.conf":   false,
		"/usr/lib/systemd/system/multi-user.target.wants/a.service": false,
		"/usr/lib/systemd/user/pipewire.service":                    false,
		"/usr/share/doc/foo/example.service":                        false,
	} {
		if got := unitFilePath(path); got != want {
			t.Errorf("unitFilePath(%q) = %v, want %v", path, got, want)
		}
	}
}

// A machine with no dpkg still says which units an administrator wrote, and nothing else.
func TestWithoutDpkgOnlyLocalUnitsAreMarked(t *testing.T) {
	idx := newPackageIndexAt(filepath.Join(t.TempDir(), "absent"))
	services := []Service{
		{Unit: "myapp.service", fragment: "/etc/systemd/system/myapp.service"},
		{Unit: "sshd.service", fragment: "/usr/lib/systemd/system/sshd.service"},
	}
	attachUnitPackages(services, idx, time.Now())
	if services[0].Local == nil || !*services[0].Local || services[0].Package != nil {
		t.Errorf("myapp: want local, got %+v", services[0])
	}
	if services[1].Local != nil || services[1].Package != nil {
		t.Errorf("sshd: nothing can be said without dpkg, got %+v", services[1])
	}

	// And with no index at all.
	services[0].Local = nil
	attachUnitPackages(services, nil, time.Now())
	if services[0].Local == nil {
		t.Error("myapp: a nil index must not stop the local mark")
	}
}

// A process's package now carries the priority and section too — the same lookup, one shape.
func TestAProcessPackageCarriesPriorityAndSection(t *testing.T) {
	idx := newPackageIndexAt(writeUnitDpkg(t))
	if !idx.refresh(time.Now()) {
		t.Fatal("no index")
	}
	p, ok := idx.lookup("/sbin/uuidd")
	if !ok {
		t.Fatal("/sbin/uuidd not found across the /usr merge")
	}
	if pkg := p.toPackage("command"); pkg.Priority != "standard" || pkg.Section != "utils" {
		t.Fatalf("got %+v", pkg)
	}
}

// PAYLOAD.md states what contract 22 adds to the one sample that carries snapshots, at the receiver's caps: 200
// services each with a package of maximum-length strings and the local mark, and the priority and section on the
// package of each of 32 processes.
func TestWhatContract22AddsToTheSnapshotSampleAtItsCaps(t *testing.T) {
	const serviceCap = 200 // maxServices, which is Linux-only; the receiver's cap is the same
	pkg := func(priority, section string) *Package {
		return &Package{
			Name: strings.Repeat("n", maxPackageName), Version: strings.Repeat("v", maxPackageVersion),
			Description: strings.Repeat("d", maxPackageDescription), Source: "dpkg", Match: "unit_file",
			Priority: priority, Section: section,
		}
	}
	priority, section := strings.Repeat("p", maxPackagePriority), strings.Repeat("s", maxPackageSection)

	without := Sample{Services: make([]Service, serviceCap), Processes: make([]Process, 32)}
	with := Sample{Services: make([]Service, serviceCap), Processes: make([]Process, 32)}
	for i := range with.Services {
		with.Services[i].Package = pkg(priority, section)
		local := true
		with.Services[i].Local = &local
	}
	for i := range with.Processes {
		without.Processes[i].Package = pkg("", "")
		with.Processes[i].Package = pkg(priority, section)
	}

	a, _ := json.Marshal(without)
	b, _ := json.Marshal(with)
	added := len(b) - len(a)
	if added > 150_936 {
		t.Fatalf("contract 22 adds %d bytes at its caps; PAYLOAD.md says 150,936", added)
	}
	t.Logf("contract 22 adds %d bytes to the snapshot sample at its caps", added)
}
