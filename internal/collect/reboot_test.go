package collect

import (
	"os"
	"path/filepath"
	"testing"
)

/*
A reboot is required on every Linux, not only Debian and Ubuntu (#3835).

Debian and Ubuntu write /var/run/reboot-required and SUSE writes /run/reboot-needed; RHEL, Alma, Rocky,
Fedora and Arch write nothing, so there the evidence is a newer kernel installed beside the running one.
The version strings below are real: the RHEL, Fedora and SUSE ones were read off /lib/modules after
installing a kernel in each distribution's own container. What is worth testing is mostly what must NOT
be called a pending reboot, because a false one costs somebody a maintenance window.
*/

func TestAnRHELKernelUpdateIsANewerKernel(t *testing.T) {
	running := "5.14.0-427.13.1.el9_4.x86_64"
	got, ok := newerKernel(running, []string{running, "5.14.0-687.57.1.el9_8.x86_64"})
	if !ok || got != "5.14.0-687.57.1.el9_8.x86_64" {
		t.Fatalf("got %q, %v", got, ok)
	}
}

func TestAFedoraKernelAcrossReleasesIsANewerKernel(t *testing.T) {
	running := "6.11.4-301.fc41.x86_64"
	if got, ok := newerKernel(running, []string{running, "6.17.10-100.fc41.x86_64"}); !ok || got != "6.17.10-100.fc41.x86_64" {
		t.Fatalf("got %q, %v", got, ok)
	}
}

func TestAnUbuntuAndASuseUpdateAreNewerKernels(t *testing.T) {
	cases := map[string][]string{
		"6.8.0-45-generic":           {"6.8.0-45-generic", "6.8.0-47-generic", "6.8.0-40-generic"},
		"6.4.0-150600.23.73-default": {"6.4.0-150600.23.73-default", "6.4.0-150600.23.103-default"},
		"6.1.0-26-amd64":             {"6.1.0-26-amd64", "6.1.0-28-amd64"},
		"6.10.5-arch1-1":             {"6.10.5-arch1-1", "6.11.2-arch1-1"},
	}
	for running, installed := range cases {
		if _, ok := newerKernel(running, installed); !ok {
			t.Errorf("%s: missed the newer kernel in %v", running, installed)
		}
	}
}

func TestTheNewestIsChosenWhenSeveralAreInstalled(t *testing.T) {
	running := "6.8.0-40-generic"
	got, _ := newerKernel(running, []string{"6.8.0-47-generic", running, "6.8.0-45-generic", "6.8.0-9-generic"})
	if got != "6.8.0-47-generic" {
		t.Fatalf("got %q", got)
	}
}

func TestRunningTheNewestKernelIsNotAPendingReboot(t *testing.T) {
	running := "6.8.0-47-generic"
	if got, ok := newerKernel(running, []string{"6.8.0-45-generic", running}); ok {
		t.Fatalf("an older kernel left installed was reported as pending: %q", got)
	}
}

func TestAnotherFlavourIsAChoiceNotAnUpdate(t *testing.T) {
	cases := map[string]string{
		"6.8.0-45-generic":             "6.8.0-47-lowlatency",
		"5.14.0-427.13.1.el9_4.x86_64": "5.14.0-503.11.1.el9_5.x86_64+debug",
		"6.1.0-26-amd64":               "6.1.0-28-rt-amd64",
	}
	for running, other := range cases {
		if got, ok := newerKernel(running, []string{running, other}); ok {
			t.Errorf("%s: %q is another flavour, reported as an update", running, got)
		}
	}
}

func TestAContainerOrUnmanagedKernelIsNotJudged(t *testing.T) {
	// Docker Desktop's kernel inside a Rocky container: /lib/modules describes the image, not the kernel.
	if got, ok := newerKernel("6.10.14-linuxkit", []string{"5.14.0-687.57.1.el9_8.aarch64"}); ok {
		t.Fatalf("judged a kernel the package manager does not own: %q", got)
	}
	// Arch removes the running kernel's modules on upgrade — a missed reboot, never a false one.
	if _, ok := newerKernel("6.10.5-arch1-1", []string{"6.11.2-arch1-1"}); ok {
		t.Fatal("judged a running kernel with no modules directory")
	}
}

func TestVersionsCompareAsNumbersNotText(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"6.10.0", "6.9.0", 1},
		{"5.14.0-503", "5.14.0-427", 1},
		{"6.8.0-9", "6.8.0-10", -1},
		{"6.8.0-045", "6.8.0-45", 0},
		{"6.8.0-45.1", "6.8.0-45", 1},
		{"el9_10", "el9_8", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// fakeMachine points the flag files and module roots at a temporary tree for one test.
func fakeMachine(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	flags, roots := rebootFlags, modulesRoots
	rebootFlags = []struct{ path, reason string }{
		{dir + "/var/run/reboot-required", "flag_file"},
		{dir + "/run/reboot-needed", "zypper"},
	}
	modulesRoots = []string{dir + "/lib/modules", dir + "/usr/lib/modules"}
	t.Cleanup(func() { rebootFlags, modulesRoots = flags, roots })

	return dir
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAKernelInstalledOnDiskIsReadAsAPendingReboot(t *testing.T) {
	dir := fakeMachine(t)
	touch(t, dir+"/lib/modules/5.14.0-427.13.1.el9_4.x86_64/modules.dep")
	touch(t, dir+"/lib/modules/5.14.0-503.11.1.el9_5.x86_64/modules.dep")

	required, reason, newest := rebootRequired("5.14.0-427.13.1.el9_4.x86_64")
	if !required || reason != "newer_kernel" || newest != "5.14.0-503.11.1.el9_5.x86_64" {
		t.Fatalf("got %v %q %q", required, reason, newest)
	}
}

func TestALeftoverDirectoryWithoutModulesDepIsNotAKernel(t *testing.T) {
	dir := fakeMachine(t)
	touch(t, dir+"/lib/modules/6.8.0-45-generic/modules.dep")
	// A removed kernel whose directory survived because a DKMS build left a file in it.
	touch(t, dir+"/lib/modules/6.8.0-50-generic/updates/dkms/zfs.ko")

	if required, _, newest := rebootRequired("6.8.0-45-generic"); required {
		t.Fatalf("a leftover directory was read as an installed kernel: %q", newest)
	}
}

func TestTheFlagFilesWinAndNameThemselves(t *testing.T) {
	dir := fakeMachine(t)
	touch(t, dir+"/run/reboot-needed")

	if required, reason, _ := rebootRequired("6.4.0-150600.23.73-default"); !required || reason != "zypper" {
		t.Fatalf("got %v %q", required, reason)
	}

	touch(t, dir+"/var/run/reboot-required")
	if _, reason, _ := rebootRequired(""); reason != "flag_file" {
		t.Fatalf("got %q", reason)
	}
}

func TestTheRootHoldingTheRunningKernelIsTheOneJudged(t *testing.T) {
	dir := fakeMachine(t)
	// Not merged-/usr: /lib/modules is a stray, the kernels live under /usr/lib/modules.
	touch(t, dir+"/lib/modules/4.18.0-1-generic/modules.dep")
	touch(t, dir+"/usr/lib/modules/6.8.0-45-generic/modules.dep")
	touch(t, dir+"/usr/lib/modules/6.8.0-47-generic/modules.dep")

	if required, _, newest := rebootRequired("6.8.0-45-generic"); !required || newest != "6.8.0-47-generic" {
		t.Fatalf("got %v %q", required, newest)
	}
}
