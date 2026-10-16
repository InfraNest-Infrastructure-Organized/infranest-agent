package collect

import (
	"os"
	"strings"
	"testing"
)

/*
How a machine patches and reboots itself (InfraNest #3825).

The APT text below is real: Ubuntu 24.04's stock 20auto-upgrades and 50unattended-upgrades as installed
with the package, and the drop-in InfraNest's own servers use to switch on the nightly reboot. What is
worth testing is APT's merge rules, because getting one wrong turns "reboots at 03:00" into "never
reboots" or the other way round.
*/

const ubuntuAutoUpgrades = `APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
`

// The relevant part of the stock file: every reboot setting is commented out.
const ubuntuUnattended = `// Automatically reboot *WITHOUT CONFIRMATION* if
//  the file /var/run/reboot-required is found after the upgrade
//Unattended-Upgrade::Automatic-Reboot "false";

// Automatically reboot even if there are users currently logged in
// when Unattended-Upgrade::Automatic-Reboot is set to true
//Unattended-Upgrade::Automatic-Reboot-WithUsers "true";

// If automatic reboot is enabled and needed, reboot at the specific
// time instead of immediately
//  Default: "now"
//Unattended-Upgrade::Automatic-Reboot-Time "02:00";
Unattended-Upgrade::Allowed-Origins {
	"${distro_id}:${distro_codename}";
	"${distro_id}:${distro_codename}-security";
};
/* a block comment
Unattended-Upgrade::Automatic-Reboot "true";
*/
`

const nightlyReboot = `Unattended-Upgrade::Automatic-Reboot "true";
Unattended-Upgrade::Automatic-Reboot-Time "03:00";
`

func conf(files ...string) map[string]string {
	c := map[string]string{}
	for _, f := range files {
		parseAptConf(strings.NewReader(f), c)
	}

	return c
}

func TestAStockUbuntuPatchesItselfButDoesNotReboot(t *testing.T) {
	au := unattendedUpgrades(conf(ubuntuAutoUpgrades, ubuntuUnattended))

	if !au.Enabled || au.Reboot || au.RebootTime != "" || au.RebootWithUsers != nil {
		t.Fatalf("got %+v", au)
	}
}

func TestANightlyRebootDropInIsRead(t *testing.T) {
	au := unattendedUpgrades(conf(ubuntuAutoUpgrades, ubuntuUnattended, nightlyReboot))

	if !au.Reboot || au.RebootTime != "03:00" || au.RebootWithUsers == nil || !*au.RebootWithUsers {
		t.Fatalf("got %+v", au)
	}
}

func TestRebootWithoutATimeIsTheToolsDefaultOfNow(t *testing.T) {
	au := unattendedUpgrades(conf(ubuntuAutoUpgrades, `Unattended-Upgrade::Automatic-Reboot "yes";`))

	if au.RebootTime != "now" {
		t.Fatalf("got %q", au.RebootTime)
	}
}

func TestTheSettingThatBlocksTheRebootIsReported(t *testing.T) {
	au := unattendedUpgrades(conf(ubuntuAutoUpgrades, nightlyReboot, `Unattended-Upgrade::Automatic-Reboot-WithUsers "false";`))

	if au.RebootWithUsers == nil || *au.RebootWithUsers {
		t.Fatalf("got %+v", au)
	}
}

func TestTheScopedFormAndCaseAreTheSameKey(t *testing.T) {
	au := unattendedUpgrades(conf(ubuntuAutoUpgrades, `unattended-upgrade {
  automatic-reboot "true";   // trailing comment
  Automatic-Reboot-Time "04:30";
};`))

	if !au.Reboot || au.RebootTime != "04:30" {
		t.Fatalf("got %+v", au)
	}
}

func TestALaterFileOverridesAnEarlierOne(t *testing.T) {
	au := unattendedUpgrades(conf(ubuntuAutoUpgrades, nightlyReboot, `Unattended-Upgrade::Automatic-Reboot "false";`))

	if au.Reboot {
		t.Fatal("the later file said false")
	}
}

func TestInstalledButSwitchedOffIsNotEnabled(t *testing.T) {
	cases := map[string]string{
		"periodic 0":   `APT::Periodic::Unattended-Upgrade "0";`,
		"never set":    ``,
		"all periodic": ubuntuAutoUpgrades + `APT::Periodic::Enable "0";`, // the Docker image's own switch-off
	}
	for name, c := range cases {
		if au := unattendedUpgrades(conf(c)); au.Enabled {
			t.Errorf("%s: reported as enabled", name)
		}
	}
}

func TestOnlyTheFilesAptReadsAreRead(t *testing.T) {
	for name, want := range map[string]bool{
		"50unattended-upgrades":           true,
		"60infranest-unattended-upgrades": true,
		"99local.conf":                    true,
		"50unattended-upgrades.ucf-dist":  false,
		"50unattended-upgrades~":          false,
		"50unattended-upgrades.dpkg-old":  false,
	} {
		if got := aptPartName(name); got != want {
			t.Errorf("%s: got %v", name, got)
		}
	}
}

func TestThePartsAreMergedInNameOrderThenTheMainFile(t *testing.T) {
	dir := t.TempDir()
	saved := []string{aptConfDir, aptConfMain}
	aptConfDir, aptConfMain = dir+"/apt.conf.d", dir+"/apt.conf"
	t.Cleanup(func() { aptConfDir, aptConfMain = saved[0], saved[1] })

	write := func(path, body string) {
		t.Helper()
		_ = os.MkdirAll(aptConfDir, 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(aptConfDir+"/20auto-upgrades", ubuntuAutoUpgrades)
	write(aptConfDir+"/60reboot", nightlyReboot)
	write(aptConfDir+"/50unattended-upgrades", `Unattended-Upgrade::Automatic-Reboot-Time "01:00";`)
	write(aptConfDir+"/60reboot.ucf-dist", `Unattended-Upgrade::Automatic-Reboot "false";`)
	write(aptConfMain, `Unattended-Upgrade::Automatic-Reboot-Time "05:15";`)

	au := unattendedUpgrades(readAptConfig())
	if !au.Reboot || au.RebootTime != "05:15" {
		t.Fatalf("got %+v", au)
	}
}

func TestDnfAutomaticNeedsATimerAsWellAsTheFile(t *testing.T) {
	ini := parseINI(strings.NewReader(`[commands]
# What kind of upgrade to perform
upgrade_type = default
apply_updates = yes
reboot = when-needed
`))

	if au := dnfAutomatic(ini, ""); au.Enabled {
		t.Fatal("no timer is enabled, so nothing ever runs")
	}
	if au := dnfAutomatic(ini, "plain"); !au.Enabled || !au.Reboot || au.RebootTime != "" {
		t.Fatalf("got %+v", au)
	}

	// The install timer applies updates whatever the file says.
	off := parseINI(strings.NewReader("[commands]\napply_updates = no\nreboot = never\n"))
	if au := dnfAutomatic(off, "install"); !au.Enabled || au.Reboot {
		t.Fatalf("got %+v", au)
	}
	if au := dnfAutomatic(off, "plain"); au.Enabled {
		t.Fatal("the plain timer only downloads when apply_updates is off")
	}
}
