package collect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func setupTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func makeUnreadable(t *testing.T, root, p string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads every file; the unreadable case needs an ordinary user")
	}
	full := filepath.Join(root, p)
	if err := os.Chmod(full, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(full, 0o755) })
}

// Ubuntu's own layout: the drop-ins are included at the top, so cloud-init's "no" beats the "yes" written below.
func TestSSHFirstValueWinsAcrossIncludes(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/ssh/sshd_config": `# This is the sshd server system-wide configuration file.
Include /etc/ssh/sshd_config.d/*.conf

#Port 22
#PermitRootLogin prohibit-password
passwordAUTHENTICATION yes
KbdInteractiveAuthentication no
UsePAM yes

Match User sftp
	PasswordAuthentication yes
	PermitRootLogin no
`,
		"etc/ssh/sshd_config.d/50-cloud-init.conf": "PasswordAuthentication no\n",
		"etc/ssh/sshd_config.d/60-port.conf":       "Port=2222\nPort 22\nPort = 2222\n",
		"etc/ssh/sshd_config.d/10-old.conf.bak":    "PasswordAuthentication yes\n",
		"etc/ssh/sshd_config.d/notes.txt":          "PermitRootLogin yes\n",
	})

	got := readSSH(root, &touchList{})
	want := &SSHSetup{
		PasswordAuthentication:       "no",
		KbdInteractiveAuthentication: "no",
		UsePAM:                       "yes",
		AuthenticationMethods:        "any",
		PermitRootLogin:              "prohibit-password",
		Ports:                        []int{2222, 22},
		SetIn: map[string]string{
			"passwordauthentication":       "/etc/ssh/sshd_config.d/50-cloud-init.conf:1",
			"kbdinteractiveauthentication": "/etc/ssh/sshd_config:7",
			"usepam":                       "/etc/ssh/sshd_config:8",
			"port":                         "/etc/ssh/sshd_config.d/60-port.conf:1",
		},
		Match: []string{"passwordauthentication", "permitrootlogin"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// The old name of the keyboard-interactive setting, quotes, a relative Include, and a value written after a comment
// that sets the same keyword.
func TestSSHAliasesQuotesAndRelativeIncludes(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/ssh/sshd_config": `Include local/*.conf
ChallengeResponseAuthentication yes
KbdInteractiveAuthentication no
PermitRootLogin "yes"
AuthenticationMethods publickey,password publickey,keyboard-interactive
PasswordAuthentication no # a trailing comment
`,
		"etc/ssh/local/a.conf": "# nothing here\n\n",
	})

	got := readSSH(root, &touchList{})
	if got.KbdInteractiveAuthentication != "yes" {
		t.Errorf("ChallengeResponseAuthentication is the same setting, first wins: got %q", got.KbdInteractiveAuthentication)
	}
	if got.PermitRootLogin != "yes" {
		t.Errorf("quotes: got %q", got.PermitRootLogin)
	}
	if got.AuthenticationMethods != "publickey,password publickey,keyboard-interactive" {
		t.Errorf("methods: got %q", got.AuthenticationMethods)
	}
	if got.PasswordAuthentication != "no" {
		t.Errorf("trailing comment: got %q", got.PasswordAuthentication)
	}
	if !reflect.DeepEqual(got.Ports, []int{22}) || got.SetIn["port"] != "" {
		t.Errorf("no Port line is sshd's default, 22: got %v %q", got.Ports, got.SetIn["port"])
	}
	if got.UsePAM != "no" {
		t.Errorf("UsePAM defaults to no: got %q", got.UsePAM)
	}
}

// A drop-in the agent cannot read could have set anything not already decided — those are unknown, never the default.
func TestSSHUnreadableFileMakesUndecidedValuesUnknown(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/ssh/sshd_config": `PermitRootLogin no
Include /etc/ssh/sshd_config.d/*.conf
PasswordAuthentication no
`,
		"etc/ssh/sshd_config.d/50-secret.conf": "PasswordAuthentication yes\n",
	})
	makeUnreadable(t, root, "etc/ssh/sshd_config.d/50-secret.conf")

	got := readSSH(root, &touchList{})
	if got.PermitRootLogin != "no" {
		t.Errorf("decided before the unread file, so still known: got %q", got.PermitRootLogin)
	}
	if got.PasswordAuthentication != "unknown" {
		t.Errorf("the unread file comes first and could set it: got %q", got.PasswordAuthentication)
	}
	if got.UsePAM != "unknown" {
		t.Errorf("a default is not a reading: got %q", got.UsePAM)
	}
	if got.Ports != nil {
		t.Errorf("any unread file may add a Port: got %v", got.Ports)
	}
	if !reflect.DeepEqual(got.Unreadable, []string{"/etc/ssh/sshd_config.d/50-secret.conf"}) {
		t.Errorf("unreadable: %v", got.Unreadable)
	}
}

func TestSSHUnreadableMainFileIsAllUnknown(t *testing.T) {
	root := setupTree(t, map[string]string{"etc/ssh/sshd_config": "PasswordAuthentication no\n"})
	makeUnreadable(t, root, "etc/ssh/sshd_config")

	got := readSSH(root, &touchList{})
	if got.PasswordAuthentication != "unknown" || got.PermitRootLogin != "unknown" || got.Ports != nil {
		t.Fatalf("got %+v", got)
	}
}

// An unreadable file inside a Match block can only change what that block changes: the global values stand.
func TestSSHUnreadableInsideMatchKeepsGlobals(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/ssh/sshd_config": `Match Address 10.0.0.0/8
	Include /etc/ssh/inner.conf
`,
		"etc/ssh/inner.conf": "PasswordAuthentication yes\n",
	})
	makeUnreadable(t, root, "etc/ssh/inner.conf")

	got := readSSH(root, &touchList{})
	if got.PasswordAuthentication != "yes" || !reflect.DeepEqual(got.Ports, []int{22}) {
		t.Fatalf("global values are sshd's defaults here: %+v", got)
	}
}

// A Match inside an included file ends with that file: what the including file says next is global again.
func TestSSHMatchEndsWithItsFile(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/ssh/sshd_config": `Include /etc/ssh/sshd_config.d/*.conf
PasswordAuthentication no
`,
		"etc/ssh/sshd_config.d/sftp.conf": "Match Group sftp\n  ForceCommand internal-sftp\n  PasswordAuthentication yes\n",
	})

	got := readSSH(root, &touchList{})
	if got.PasswordAuthentication != "no" {
		t.Errorf("got %q", got.PasswordAuthentication)
	}
	if !reflect.DeepEqual(got.Match, []string{"passwordauthentication"}) {
		t.Errorf("match: %v", got.Match)
	}
}

func TestSSHAbsentWithoutAServer(t *testing.T) {
	if got := readSSH(t.TempDir(), &touchList{}); got != nil {
		t.Fatalf("no sshd_config is no SSH server: %+v", got)
	}
}

func TestAdminGroups(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/group": `root:x:0:
sudo:x:27:bob,alice, carol
admin:x:115:
docker:x:999:alice
sudo:x:28:mallory
`,
		"etc/passwd": `root:x:0:0:root:/root:/bin/bash
deploy:x:1001:115::/home/deploy:/bin/bash
alice:x:1000:1000::/home/alice:/bin/bash
`,
	})

	got := readAdminGroups(root, &touchList{})
	want := &[]AdminGroup{
		{Group: "sudo", Members: []string{"alice", "bob", "carol"}},
		{Group: "admin", Members: []string{"deploy"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", *got)
	}
}

func TestAdminGroupsAbsentWhenGroupFileUnreadable(t *testing.T) {
	if got := readAdminGroups(t.TempDir(), &touchList{}); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestAptUpdatesLastValueWins(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/debian_version": "12.7\n",
		"etc/apt/apt.conf.d/20auto-upgrades": `APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
`,
		"etc/apt/apt.conf.d/99-off":                         "APT { Periodic { Unattended-Upgrade \"0\"; }; };\n",
		"etc/apt/apt.conf.d/50unattended-upgrades.ucf-dist": "APT::Periodic::Unattended-Upgrade \"1\";\n",
		"var/lib/dpkg/status": `Package: unattended-upgrades
Status: install ok installed
Version: 2.9.1

Package: apt
Status: install ok installed
`,
	})

	got := readUpdates(root, &touchList{})
	installed := true
	want := &UpdatesSetup{Family: "debian", Tool: "unattended-upgrades", Installed: &installed, Setting: "0", SetIn: "/etc/apt/apt.conf.d/99-off"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestAptUpdatesNotInstalled(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/debian_version":  "12.7\n",
		"var/lib/dpkg/status": "Package: unattended-upgrades\nStatus: deinstall ok config-files\n",
	})

	got := readUpdates(root, &touchList{})
	if got.Installed == nil || *got.Installed || got.Setting != "" {
		t.Fatalf("got %+v", got)
	}
}

func TestDnfUpdates(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/redhat-release":     "AlmaLinux release 9.4\n",
		"etc/dnf/automatic.conf": "[commands]\n# apply_updates = no\napply_updates = yes\n",
		"etc/systemd/system/timers.target.wants/dnf-automatic.timer": "",
	})

	got := readUpdates(root, &touchList{})
	installed := true
	want := &UpdatesSetup{Family: "rhel", Tool: "dnf-automatic", Installed: &installed, Setting: "yes", SetIn: "/etc/dnf/automatic.conf", Timer: "plain"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestUpdatesAbsentElsewhere(t *testing.T) {
	if got := readUpdates(t.TempDir(), &touchList{}); got != nil {
		t.Fatalf("got %+v", got)
	}
}

// Cached until a file it read changes — including a drop-in that appears in a folder it globbed.
func TestSetupReaderCachesUntilAFileChanges(t *testing.T) {
	root := setupTree(t, map[string]string{
		"etc/ssh/sshd_config":                   "Include /etc/ssh/sshd_config.d/*.conf\nPasswordAuthentication yes\n",
		"etc/ssh/sshd_config.d/placeholder.txt": "",
		"etc/group":                             "sudo:x:27:alice\n",
	})
	r := NewSetupReader()

	first := r.Read(root)
	if first == nil || first.SSH.PasswordAuthentication != "yes" {
		t.Fatalf("first: %+v", first)
	}
	if again := r.Read(root); again != first {
		t.Fatal("nothing changed, so the cached answer is returned")
	}

	dir := filepath.Join(root, "etc/ssh/sshd_config.d")
	if err := os.WriteFile(filepath.Join(dir, "50-cloud-init.conf"), []byte("PasswordAuthentication no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(dir, later, later)

	if got := r.Read(root); got.SSH.PasswordAuthentication != "no" {
		t.Fatalf("a new drop-in is read: %+v", got.SSH)
	}

	group := filepath.Join(root, "etc/group")
	if err := os.WriteFile(group, []byte("sudo:x:27:alice,deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(group, later.Add(time.Second), later.Add(time.Second))
	if got := r.Read(root); !reflect.DeepEqual((*got.AdminGroups)[0].Members, []string{"alice", "deploy"}) {
		t.Fatalf("a changed /etc/group is read: %+v", got.AdminGroups)
	}
}

func TestSetupNilWhenNothingRead(t *testing.T) {
	if got := NewSetupReader().Read(t.TempDir()); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestSSHLine(t *testing.T) {
	cases := map[string][]string{
		"Port 22":                  {"port", "22"},
		"Port=22":                  {"port", "22"},
		"Port = 22":                {"port", "22"},
		"  PermitRootLogin\tno  ":  {"permitrootlogin", "no"},
		`Banner "/etc/my banner"`:  {"banner", "/etc/my banner"},
		"UsePAM yes # comment":     {"usepam", "yes"},
		"# PasswordAuthentication": nil,
		"":                         nil,
	}
	for line, want := range cases {
		k, args := sshLine(line)
		var got []string
		if k != "" {
			got = append([]string{k}, args...)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v, want %v", line, got, want)
		}
	}
}
