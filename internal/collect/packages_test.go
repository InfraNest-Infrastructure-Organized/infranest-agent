package collect

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dpkgStatusFixture = `Package: nginx-core
Status: install ok installed
Priority: optional
Architecture: amd64
Version: 1.24.0-2ubuntu7.1
Description: nginx web/proxy server (standard version)
 Nginx ("engine X") is a high-performance web and reverse proxy server
 created by Igor Sysoev.

Package: postgresql-16
Status: install ok installed
Architecture: amd64
Version: 16.4-0ubuntu0.24.04.2
Conffiles:
 /etc/postgresql-common/foo 1234
Description: The World's Most Advanced Open Source Relational Database

Package: libc-bin
Status: install ok installed
Architecture: amd64
Multi-Arch: foreign
Version: 2.39-0ubuntu8.3
Description: GNU C Library: Binaries

Package: oldtool
Status: deinstall ok config-files
Architecture: amd64
Version: 1.0
Description: removed, only its configuration left

Package: libfoo1
Status: install ok installed
Architecture: arm64
Multi-Arch: same
Version: 3.1
Description: a library installed for one foreign architecture

Package: busybox-a
Status: install ok installed
Architecture: amd64
Version: 1
Description: one of two packages shipping a program called tool

Package: busybox-b
Status: install ok installed
Architecture: amd64
Version: 2
Description: the other
`

// writeDpkg lays out a fixture dpkg database: the status file and one file list per package.
func writeDpkg(t *testing.T) string {
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
	write("status", dpkgStatusFixture)
	write("info/nginx-core.list", "/.\n/usr\n/usr/sbin\n/usr/sbin/nginx\n/usr/share/doc/nginx-core/copyright\n")
	write("info/postgresql-16.list", "/usr/lib/postgresql/16/bin\n/usr/lib/postgresql/16/bin/postgres\n/usr/lib/postgresql/16/bin/pg_ctl\n/usr/lib/postgresql/16/lib/plpgsql.so\n")
	// Listed under /sbin, as a package built before the /usr merge lists it.
	write("info/libc-bin.list", "/sbin/ldconfig\n/usr/bin/getconf\n")
	write("info/oldtool.list", "/usr/bin/oldtool\n")
	write("info/libfoo1:arm64.list", "/usr/libexec/foo/helper\n")
	write("info/busybox-a.list", "/usr/bin/tool\n")
	write("info/busybox-b.list", "/bin/tool\n")

	return dir
}

func TestOnlyInstalledPackagesAreReadWithTheFirstLineOfTheirDescription(t *testing.T) {
	pkgs := parseDpkgStatus(strings.NewReader(dpkgStatusFixture))

	if p := pkgs["nginx-core"]; p.version != "1.24.0-2ubuntu7.1" || p.description != "nginx web/proxy server (standard version)" {
		t.Fatalf("%+v", p)
	}
	if _, ok := pkgs["oldtool"]; ok {
		t.Fatal("a removed package whose configuration is left is not installed")
	}
	if _, ok := pkgs["libfoo1:arm64"]; !ok {
		t.Fatal("a multi-arch package is keyed by name and architecture, as its file list is named")
	}
}

func TestOnlyExecutablesAreIndexed(t *testing.T) {
	for path, want := range map[string]bool{
		"/usr/sbin/nginx":                     true,
		"/usr/lib/postgresql/16/bin/postgres": true,
		"/usr/libexec/foo/helper":             true,
		"/usr/sbin":                           false, // the directory itself
		"/usr/share/doc/nginx-core/copyright": false,
		"/usr/lib/postgresql/16/lib/x.so":     false,
		"relative/bin/x":                      false,
	} {
		if got := executablePath(path); got != want {
			t.Errorf("%s: %v", path, got)
		}
	}
}

func TestAPathIsPlacedOnItsPackageAcrossTheUsrMerge(t *testing.T) {
	idx := newPackageIndexAt(writeDpkg(t))
	if !idx.refresh(time.Now()) {
		t.Fatal("no index")
	}

	cases := map[string]string{
		"/usr/sbin/nginx":                     "nginx-core",
		"/usr/lib/postgresql/16/bin/postgres": "postgresql-16",
		"/usr/sbin/ldconfig":                  "libc-bin", // listed as /sbin/ldconfig
		"/usr/libexec/foo/helper":             "libfoo1",
	}
	for path, want := range cases {
		p, ok := idx.lookup(path)
		if !ok || p.name != want {
			t.Errorf("%s: %+v %v", path, p, ok)
		}
	}
	if _, ok := idx.lookup("/usr/bin/oldtool"); ok {
		t.Error("a removed package owns nothing")
	}
}

func TestANameIsMatchedOnlyWhenOnePackageHasIt(t *testing.T) {
	idx := newPackageIndexAt(writeDpkg(t))
	idx.refresh(time.Now())

	if p, ok := idx.lookupName("nginx"); !ok || p.name != "nginx-core" {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := idx.lookupName("tool"); ok {
		t.Fatal("two packages ship a program called tool: the name says neither")
	}
	// Not in a program directory: never matched by name, only by path.
	if _, ok := idx.lookupName("postgres"); ok {
		t.Fatal("postgres lives under /usr/lib, not where a shell looks")
	}
}

func TestTheIndexIsRebuiltOnlyWhenTheDatabaseChanges(t *testing.T) {
	dir := writeDpkg(t)
	idx := newPackageIndexAt(dir)
	start := time.Now()
	idx.refresh(start)
	first := idx.builtAt

	idx.refresh(start.Add(time.Hour))
	if !idx.builtAt.Equal(first) {
		t.Fatal("an unchanged database is not read again")
	}

	// An upgrade: a new version, and the status file's time moves.
	body := strings.Replace(dpkgStatusFixture, "1.24.0-2ubuntu7.1", "1.24.0-2ubuntu7.2", 1)
	status := filepath.Join(dir, "status")
	if err := os.WriteFile(status, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(status, later, later); err != nil {
		t.Fatal(err)
	}

	// Within the rebuild floor of the last build: the old answer stands.
	idx.refresh(first.Add(time.Minute))
	if p, _ := idx.lookup("/usr/sbin/nginx"); p.version != "1.24.0-2ubuntu7.1" {
		t.Fatalf("rebuilt inside the floor: %+v", p)
	}

	idx.refresh(first.Add(minIndexRebuild + time.Second))
	if p, _ := idx.lookup("/usr/sbin/nginx"); p.version != "1.24.0-2ubuntu7.2" {
		t.Fatalf("not rebuilt after the change: %+v", p)
	}
}

func TestAMachineWithoutDpkgHasNoIndexAndNoFailure(t *testing.T) {
	idx := newPackageIndexAt(filepath.Join(t.TempDir(), "missing"))
	procs := []Process{{Command: "nginx", PID: 1}}

	attachPackages(procs, idx, t.TempDir(), time.Now())

	if procs[0].Package != nil {
		t.Fatalf("%+v", procs[0].Package)
	}
}

// fakeProc writes /proc/<pid>/{stat,cmdline} and, when exe is set, the exe link.
func fakeProc(t *testing.T, root string, pid int, comm, cmdline, exe string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) S 1 %d %d 0 -1 4194560 100 0 0 0 5 3 0 0 20 0 1 0 1000 1000000 200 18446744073709551615", pid, comm, pid, pid)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o444); err != nil {
		t.Fatal(err)
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEachProcessIsPlacedByTheStrongestEvidenceItHas(t *testing.T) {
	idx := newPackageIndexAt(writeDpkg(t))
	proc := t.TempDir()

	// The agent's own kind of process: the exe link is readable.
	fakeProc(t, proc, 10, "nginx", "nginx: master process /usr/sbin/nginx\x00", "/usr/sbin/nginx")
	// Another user's: no exe link, started by its full path.
	fakeProc(t, proc, 11, "postgres", "/usr/lib/postgresql/16/bin/postgres\x00-D\x00/var/lib/postgresql/16/main\x00", "")
	// A postgres child that rewrote its title: no path anywhere, and postgres is not in a program directory.
	fakeProc(t, proc, 12, "postgres", "postgres: 16/main: checkpointer \x00", "")
	// Started by name from PATH: matched by name.
	fakeProc(t, proc, 13, "nginx", "nginx: worker process\x00", "")
	// Running a file an upgrade has since replaced.
	fakeProc(t, proc, 14, "nginx", "nginx\x00", "/usr/sbin/nginx (deleted)")
	// A command line claiming a path whose name is not the process's own.
	fakeProc(t, proc, 15, "evil", "/usr/sbin/nginx\x00", "")
	// In a container: its /usr/sbin/nginx is the image's, not this machine's.
	fakeProc(t, proc, 16, "nginx", "/usr/sbin/nginx\x00", "")

	procs := []Process{
		{Command: "nginx", PID: 10}, {Command: "postgres", PID: 11}, {Command: "postgres", PID: 12},
		{Command: "nginx", PID: 13}, {Command: "nginx", PID: 14}, {Command: "evil", PID: 15},
		{Command: "nginx", PID: 16, ContainerID: "3f2a1c9b8e7d"}, {Command: "gone", PID: 17},
	}
	attachPackages(procs, idx, proc, time.Now())

	want := []string{
		"nginx-core 1.24.0-2ubuntu7.1 executable",
		"postgresql-16 16.4-0ubuntu0.24.04.2 command",
		"",
		"nginx-core 1.24.0-2ubuntu7.1 name",
		"",
		"",
		"",
		"",
	}
	for i, p := range procs {
		got := ""
		if p.Package != nil {
			got = p.Package.Name + " " + p.Package.Version + " " + p.Package.Match
			if p.Package.Source != "dpkg" {
				t.Errorf("pid %d source %q", p.PID, p.Package.Source)
			}
		}
		if got != want[i] {
			t.Errorf("pid %d: %q, want %q", p.PID, got, want[i])
		}
	}

	body, _ := json.Marshal(procs[0])
	if !strings.Contains(string(body), `"package":{"name":"nginx-core","version":"1.24.0-2ubuntu7.1","description":"nginx web/proxy server (standard version)","source":"dpkg","match":"executable"}`) {
		t.Fatalf("%s", body)
	}
	// The command line read to find the file is never what is sent.
	if strings.Contains(string(body), "master process") {
		t.Fatalf("%s", body)
	}
}

// syntheticDpkg writes a database the size of a busy server: 2,000 packages of 50 files each, a fifth of them
// executables — what TestBuildingTheIndexIsCheap and the benchmark measure.
func syntheticDpkg(tb testing.TB, packages, files int) string {
	tb.Helper()
	dir := tb.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "info"), 0o755); err != nil {
		tb.Fatal(err)
	}
	var status strings.Builder
	for i := 0; i < packages; i++ {
		name := fmt.Sprintf("package-%04d", i)
		fmt.Fprintf(&status, "Package: %s\nStatus: install ok installed\nArchitecture: amd64\nVersion: 1.%d-1ubuntu1\nDescription: synthetic package number %d\n long description line one\n long description line two\n\n", name, i, i)
		var list strings.Builder
		for f := 0; f < files; f++ {
			if f%5 == 0 {
				fmt.Fprintf(&list, "/usr/bin/%s-tool-%d\n", name, f)
			} else {
				fmt.Fprintf(&list, "/usr/share/%s/data/file-%d.txt\n", name, f)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "info", name+".list"), []byte(list.String()), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status.String()), 0o644); err != nil {
		tb.Fatal(err)
	}

	return dir
}

func TestBuildingTheIndexIsCheap(t *testing.T) {
	dir := syntheticDpkg(t, 2000, 50)
	idx := newPackageIndexAt(dir)

	start := time.Now()
	idx.refresh(start)
	took := time.Since(start)

	if len(idx.byPath) != 2000*10 {
		t.Fatalf("%d paths indexed", len(idx.byPath))
	}
	// A loose ceiling — CI machines vary — that still catches an accidental quadratic.
	if took > 5*time.Second {
		t.Fatalf("building the index took %s", took)
	}
	t.Logf("2,000 packages, 100,000 files: index built in %s, %d executables indexed", took, len(idx.byPath))

	// And the steady state: one stat.
	start = time.Now()
	for i := 0; i < 100; i++ {
		idx.refresh(start)
	}
	t.Logf("an unchanged database: %s per collection", time.Since(start)/100)
}

func BenchmarkPackageIndexBuild(b *testing.B) {
	dir := syntheticDpkg(b, 2000, 50)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := newPackageIndexAt(dir)
		idx.refresh(time.Now())
	}
}
