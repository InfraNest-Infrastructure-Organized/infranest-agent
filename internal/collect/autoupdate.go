package collect

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// AutoUpdate is how this machine is set up to patch, and reboot, itself (InfraNest #3825).
//
// A server that installs updates and reboots on its own schedule is judged differently from one that
// waits for a person: a pending reboot on it is not news until the scheduled reboot has been missed, and
// the interesting failure is exactly that — most often Automatic-Reboot-WithUsers "false" and a forgotten
// SSH session, blocking every nightly reboot while the owner believes the fleet patches itself.
//
// Read from the tools' own configuration, which is world-readable. Nothing is run.
type AutoUpdate struct {
	// `unattended-upgrades` or `dnf-automatic`.
	Source string `json:"source"`

	// It installs updates on its own schedule. False when the tool is installed but switched off — which
	// is worth sending: "installed, never runs" is the setup people believe they have and do not.
	Enabled bool `json:"enabled"`

	// It reboots by itself when an update needs one.
	Reboot bool `json:"reboot"`

	// When, as the tool says it: "03:00" (the machine's local time) or "now" for unattended-upgrades;
	// absent for dnf-automatic, which reboots when its timer runs.
	RebootTime string `json:"reboot_time,omitempty"`

	// unattended-upgrades only: whether it reboots while somebody is logged in. False is the setting
	// that quietly blocks the reboot.
	RebootWithUsers *bool `json:"reboot_with_users,omitempty"`

	// The machine's offset from UTC, in minutes, so a local reboot time can be placed on the receiver's
	// clock. From the zone Go reads out of /etc/localtime — the same zone the tool schedules in.
	UTCOffsetMinutes int `json:"utc_offset_minutes"`
}

// Where each tool keeps its configuration, and how we know it is installed. Variables so tests can point
// them at a temporary tree.
var (
	aptConfDir     = "/etc/apt/apt.conf.d"
	aptConfMain    = "/etc/apt/apt.conf"
	unattendedBins = []string{"/usr/bin/unattended-upgrade", "/usr/bin/unattended-upgrades"}
	dnfAutoConf    = []string{"/etc/dnf/automatic.conf", "/etc/dnf/dnf5-plugins/automatic.conf"}
	timerWantsDirs = []string{"/etc/systemd/system/timers.target.wants", "/usr/lib/systemd/system/timers.target.wants"}
)

// Bounds on what is read: apt.conf.d holds a dozen small files on a real machine.
const (
	maxAptFiles    = 200
	maxConfigBytes = 256 << 10
)

// CollectAutoUpdate reports the first tool that is installed, or nil when neither is.
func CollectAutoUpdate() *AutoUpdate {
	_, offset := time.Now().Zone()

	if anyExists(unattendedBins) {
		au := unattendedUpgrades(readAptConfig())
		au.UTCOffsetMinutes = offset / 60

		return &au
	}

	for _, path := range dnfAutoConf {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		au := dnfAutomatic(parseINI(io.LimitReader(f, maxConfigBytes)), dnfTimerEnabled())
		_ = f.Close()
		au.UTCOffsetMinutes = offset / 60

		return &au
	}

	return nil
}

func anyExists(paths []string) bool {
	for _, p := range paths {
		if fileExists(p) {
			return true
		}
	}

	return false
}

// unattendedUpgrades reads the settings out of merged APT configuration (keys lower-cased).
func unattendedUpgrades(conf map[string]string) AutoUpdate {
	au := AutoUpdate{Source: "unattended-upgrades"}

	// APT::Periodic::Unattended-Upgrade is "1" for daily, a number of days, or "always"; "0" or unset
	// is off. APT::Periodic::Enable "0" switches every periodic task off at once.
	periodic := conf["apt::periodic::unattended-upgrade"]
	au.Enabled = periodic != "" && periodic != "0" && conf["apt::periodic::enable"] != "0"

	au.Reboot = aptTrue(conf["unattended-upgrade::automatic-reboot"])
	if au.Reboot {
		au.RebootTime = conf["unattended-upgrade::automatic-reboot-time"]
		if au.RebootTime == "" {
			au.RebootTime = "now" // the tool's own default
		}
		au.RebootTime = clip(au.RebootTime, maxState)

		withUsers := true // the tool's own default
		if v, ok := conf["unattended-upgrade::automatic-reboot-withusers"]; ok {
			withUsers = aptTrue(v)
		}
		au.RebootWithUsers = &withUsers
	}

	return au
}

func aptTrue(v string) bool {
	switch strings.ToLower(v) {
	case "true", "yes", "on", "1", "enable":
		return true
	}

	return false
}

// readAptConfig merges APT's configuration the way APT does: the parts directory in name order, then the
// main file, a later assignment replacing an earlier one.
func readAptConfig() map[string]string {
	conf := map[string]string{}

	entries, _ := os.ReadDir(aptConfDir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && aptPartName(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) > maxAptFiles {
		names = names[:maxAptFiles]
	}

	for _, name := range append(prefixAll(aptConfDir+"/", names), aptConfMain) {
		f, err := os.Open(name)
		if err != nil {
			continue
		}
		parseAptConf(io.LimitReader(f, maxConfigBytes), conf)
		_ = f.Close()
	}

	return conf
}

func prefixAll(prefix string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = prefix + n
	}

	return out
}

// aptPartName is APT's own rule for which files in apt.conf.d it reads: letters, digits, '_', '-' and
// '.', with no extension or ".conf". That is what skips "50unattended-upgrades.ucf-dist" and editor
// backups, which would otherwise override the real file.
func aptPartName(name string) bool {
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	if ext := filepath.Ext(name); ext != "" && ext != ".conf" {
		return false
	}

	return name != ""
}

// parseAptConf reads APT's configuration syntax into conf, keys lower-cased and joined with "::".
//
//	Unattended-Upgrade::Automatic-Reboot "true";
//	Unattended-Upgrade { Automatic-Reboot-Time "03:00"; };
//	Unattended-Upgrade::Allowed-Origins { "${distro_id}:${distro_codename}-security"; };
//
// `//` and `/* */` comments are dropped, and so is a line starting with '#' (APT's #clear and #include
// directives, which this does not follow). List items — a bare string inside a block — are skipped: none
// of the settings read here is a list.
func parseAptConf(r io.Reader, conf map[string]string) {
	toks := aptTokens(r)
	var scope []string

	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == '}':
			if len(scope) > 0 {
				scope = scope[:len(scope)-1]
			}
		case t.kind == ';' || t.kind == '"':
			// a stray terminator, or a list item
		case i+1 < len(toks) && toks[i+1].kind == '{':
			scope = append(scope, strings.ToLower(t.text))
			i++
		case i+1 < len(toks) && toks[i+1].kind == '"':
			key := strings.ToLower(strings.Join(append(append([]string{}, scope...), t.text), "::"))
			conf[key] = toks[i+1].text
			i++
		}
	}
}

type aptToken struct {
	kind byte // 'w' word, '"' string, '{', '}', ';'
	text string
}

func aptTokens(r io.Reader) []aptToken {
	var out []aptToken
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxConfigBytes)
	inBlock := false

	for scanner.Scan() {
		line := scanner.Text()
		if !inBlock && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		for i := 0; i < len(line); {
			c := line[i]
			switch {
			case inBlock:
				if end := strings.Index(line[i:], "*/"); end >= 0 {
					inBlock = false
					i += end + 2
				} else {
					i = len(line)
				}
			case strings.HasPrefix(line[i:], "//"):
				i = len(line)
			case strings.HasPrefix(line[i:], "/*"):
				inBlock = true
				i += 2
			case c == '"':
				end := strings.IndexByte(line[i+1:], '"')
				if end < 0 {
					end = len(line) - i - 1
				}
				out = append(out, aptToken{'"', line[i+1 : i+1+end]})
				i += end + 2
			case c == '{' || c == '}' || c == ';':
				out = append(out, aptToken{kind: c})
				i++
			case c == ' ' || c == '\t' || c == '\r':
				i++
			default:
				j := i
				for j < len(line) && !strings.ContainsRune(" \t\r{};\"", rune(line[j])) && !strings.HasPrefix(line[j:], "//") {
					j++
				}
				out = append(out, aptToken{'w', line[i:j]})
				i = j
			}
		}
	}

	return out
}

// dnfAutomatic reads dnf-automatic's settings. It only runs from a timer, so "enabled" also needs one of
// its timers switched on — the config file alone is shipped with every install.
func dnfAutomatic(ini map[string]string, timer string) AutoUpdate {
	au := AutoUpdate{Source: "dnf-automatic"}

	// dnf-automatic-install.timer applies updates whatever the file says; the plain timer only when
	// apply_updates is on. dnf-automatic-download/notify never install.
	apply := iniTrue(ini["commands.apply_updates"])
	au.Enabled = timer == "install" || (timer == "plain" && apply)

	switch strings.ToLower(ini["commands.reboot"]) {
	case "when-needed", "when-changed":
		au.Reboot = true
	}

	return au
}

func iniTrue(v string) bool {
	switch strings.ToLower(v) {
	case "yes", "true", "1", "on":
		return true
	}

	return false
}

// dnfTimerEnabled says which dnf-automatic timer is enabled, from the symlinks `systemctl enable` makes.
func dnfTimerEnabled() string {
	for _, dir := range timerWantsDirs {
		switch {
		case fileExists(dir + "/dnf-automatic-install.timer"), fileExists(dir + "/dnf5-automatic-install.timer"):
			return "install"
		case fileExists(dir + "/dnf-automatic.timer"), fileExists(dir + "/dnf5-automatic.timer"):
			return "plain"
		}
	}

	return ""
}

// parseINI reads `[section]` and `key = value` lines into "section.key", lower-cased.
func parseINI(r io.Reader) map[string]string {
	out := map[string]string{}
	section := ""
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
		default:
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			out[section+"."+strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}

	return out
}
