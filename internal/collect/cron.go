package collect

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// CronJob is one job cron runs on this machine (InfraNest #5801): a line of the system crontab or of a file in
// /etc/cron.d, or a script in one of the run-parts folders.
//
// Only what any user may read. /etc/crontab, /etc/cron.d and the run-parts folders are world-readable on Debian,
// Ubuntu and the RHEL family alike; the users' own crontabs (`crontab -e`, /var/spool/cron) are readable by root
// alone and are never opened — the agent does not run as root, and asking would only collect refusals.
//
// The command itself is sent only when command lines are switched on (`INFRANEST_PROCESS_ARGS`). A cron line is
// where people put the database password, so the default is the program's name and nothing after it.
type CronJob struct {
	// The file the job is defined in: /etc/crontab, /etc/cron.d/<name>, or the script itself for a run-parts folder.
	Source string `json:"source"`
	// The line number in Source, from 1. Absent for a run-parts script, which is a whole file.
	Line int `json:"line,omitempty"`
	// When it runs, as cron reads it: five fields ("15 3 * * *") or a nickname ("@daily", "@reboot"). For a run-parts
	// script, the schedule of the line that runs its folder, or the folder's nickname when anacron runs it. Absent
	// when nothing on the machine says when the folder runs.
	Schedule string `json:"schedule,omitempty"`
	// The user the job runs as — the system crontab's sixth field. Run-parts scripts run as root.
	User string `json:"user,omitempty"`
	// The program the job runs, by its file name: `backup.sh`, `sessionclean`. Never its arguments.
	Program string `json:"program"`
	// The whole command, only when command lines are switched on.
	Command string `json:"command,omitempty"`
	// hourly, daily, weekly or monthly: a script in /etc/cron.<folder>.
	Folder string `json:"folder,omitempty"`
	// The folder is run by anacron, which picks the time within its own window: the schedule is a period, not a time.
	Anacron bool `json:"anacron,omitempty"`
	// The time zone the schedule is read in: the file's CRON_TZ, else the machine's own (/etc/localtime). Absent when
	// neither could be read, which is not "UTC".
	Timezone string `json:"timezone,omitempty"`
	// The job's name when the line runs through `infranest-agent run --job NAME`: what joins it to the reports that
	// wrapper writes. A name the customer chose, so sent whether or not command lines are on.
	Job string `json:"job,omitempty"`
}

// Bounds, so an /etc/cron.d somebody filled by mistake costs a clipped list rather than a refused push.
const (
	maxCronJobs      = 200
	maxCronFiles     = 100
	maxCronFileBytes = 64 << 10
	maxCronSource    = 255
	maxCronSchedule  = 64
	maxCronUser      = 64
	maxCronProgram   = 128
	maxCronCommand   = 1024
	maxCronZone      = 64
	maxCronJob       = 128
)

// CronAvailable is whether this platform has a cron the agent reads: Linux — Debian and Ubuntu's cron and the RHEL
// family's cronie keep their jobs in the same places.
func CronAvailable() bool { return runtime.GOOS == "linux" }

// CronFolders are the run-parts folders, in the order cron runs them.
var CronFolders = []string{"hourly", "daily", "weekly", "monthly"}

// CronReader reads the cron definitions and keeps the answer until a file changes (InfraNest #5801). Held by the run
// loop for the life of the process: the files change a few times a year, and re-reading them every minute is a
// directory walk for nothing. Every file and folder is stat'ed each time, which is what notices the change.
type CronReader struct {
	mu          sync.Mutex
	fingerprint string
	jobs        []CronJob
}

// NewCronReader is a reader with nothing cached.
func NewCronReader() *CronReader {
	return &CronReader{}
}

// Read answers from root ("/" outside tests). Never nil: an empty list is "no cron jobs here".
func (r *CronReader) Read(root string, args bool) []CronJob {
	r.mu.Lock()
	defer r.mu.Unlock()

	fp := cronFingerprint(root) + fmt.Sprintf("|args=%t", args)
	if r.jobs != nil && fp == r.fingerprint {
		return r.jobs
	}
	r.jobs = readCron(root, args)
	r.fingerprint = fp

	return r.jobs
}

// cronFingerprint is every file the answer depends on, with its size and modification time.
func cronFingerprint(root string) string {
	var b strings.Builder
	note := func(path string) {
		if fi, err := os.Stat(filepath.Join(root, path)); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", path, fi.Size(), fi.ModTime().UnixNano())
		}
	}
	if target, err := os.Readlink(filepath.Join(root, "etc/localtime")); err == nil {
		b.WriteString("tz=" + target + ";")
	}
	for _, f := range []string{"etc/crontab", "etc/anacrontab", "etc/timezone", "usr/sbin/anacron", "etc/debian_version"} {
		note(f)
	}
	for _, dir := range append([]string{"d"}, CronFolders...) {
		rel := "etc/cron." + dir
		note(rel)
		entries, _ := os.ReadDir(filepath.Join(root, rel))
		for i, e := range entries {
			if i >= maxCronFiles {
				break
			}
			note(rel + "/" + e.Name())
		}
	}

	return b.String()
}

// readCron reads every definition once. The run-parts lines are not jobs of their own — "run everything in
// /etc/cron.daily at 06:25" is how the scripts in that folder get their time — so they are read for that and the
// scripts are listed instead.
func readCron(root string, args bool) []CronJob {
	zone := systemZone(root)
	debian := exists(filepath.Join(root, "etc/debian_version"))
	anacron := exists(filepath.Join(root, "usr/sbin/anacron"))

	var lines []cronLine
	lines = append(lines, readCrontab(root, "/etc/crontab")...)
	for _, name := range cronFiles(filepath.Join(root, "etc/cron.d"), debian, false) {
		lines = append(lines, readCrontab(root, "/etc/cron.d/"+name)...)
	}

	jobs := []CronJob{}
	folders := map[string]cronLine{}
	for _, l := range lines {
		if folder, ok := runPartsFolder(l.command); ok {
			// Debian's /etc/crontab hands the daily, weekly and monthly folders to anacron when it is installed:
			// `test -x /usr/sbin/anacron || ( cd / && run-parts --report /etc/cron.daily )`.
			l.anacron = anacron && strings.Contains(l.command, "anacron")
			if _, seen := folders[folder]; !seen {
				folders[folder] = l
			}

			continue
		}
		jobs = append(jobs, l.job(zone, args))
	}

	// The RHEL family runs the daily, weekly and monthly folders from anacron alone (/etc/anacrontab), with no
	// crontab line naming them.
	anacrontab := readFile(filepath.Join(root, "etc/anacrontab"))
	for _, folder := range CronFolders {
		dir := filepath.Join(root, "etc/cron."+folder)
		scripts := cronFiles(dir, debian, true)
		if len(scripts) == 0 {
			continue
		}
		line, known := folders[folder]
		schedule, byAnacron := "", false
		switch {
		case known && line.anacron:
			schedule, byAnacron = "@"+folder, true
		case known:
			schedule = line.schedule
		case strings.Contains(anacrontab, "cron."+folder):
			schedule, byAnacron = "@"+folder, true
		}
		for _, name := range scripts {
			jobs = append(jobs, CronJob{
				Source:   clip("/etc/cron."+folder+"/"+name, maxCronSource),
				Schedule: clip(schedule, maxCronSchedule),
				User:     "root",
				Program:  clip(name, maxCronProgram),
				Folder:   folder,
				Anacron:  byAnacron,
				Timezone: clip(zone, maxCronZone),
			})
		}
	}

	if len(jobs) > maxCronJobs {
		jobs = jobs[:maxCronJobs]
	}

	return jobs
}

// cronLine is one job line before it is made into what is sent.
type cronLine struct {
	source   string
	line     int
	schedule string
	user     string
	command  string
	zone     string // CRON_TZ in force for this line
	anacron  bool
}

func (l cronLine) job(systemZone string, args bool) CronJob {
	zone := l.zone
	if zone == "" {
		zone = systemZone
	}
	name, program := wrappedJob(l.command)
	if program == "" {
		program = programOf(l.command)
	}
	j := CronJob{
		Source:   clip(l.source, maxCronSource),
		Line:     l.line,
		Schedule: clip(l.schedule, maxCronSchedule),
		User:     clip(l.user, maxCronUser),
		Program:  clip(program, maxCronProgram),
		Timezone: clip(zone, maxCronZone),
		Job:      clip(name, maxCronJob),
	}
	if args {
		j.Command = clip(l.command, maxCronCommand)
	}

	return j
}

var (
	cronEnvLine  = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$`)
	cronNickname = map[string]bool{"@reboot": true, "@yearly": true, "@annually": true, "@monthly": true, "@weekly": true, "@daily": true, "@midnight": true, "@hourly": true}
	cronUser     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\$?$`)
)

// readCrontab reads one system crontab: five fields (or a nickname), the user, the command. A line cron itself
// would refuse is skipped rather than guessed at.
func readCrontab(root, path string) []cronLine {
	f, err := os.Open(filepath.Join(root, path))
	if err != nil {
		return nil
	}
	defer f.Close()

	return parseCrontab(io.LimitReader(f, maxCronFileBytes), path)
}

func parseCrontab(r io.Reader, path string) []cronLine {
	var out []cronLine
	zone := ""
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxCronFileBytes)
	n := 0
	for sc.Scan() {
		n++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if m := cronEnvLine.FindStringSubmatch(text); m != nil && !strings.HasPrefix(text, "@") {
			if m[1] == "CRON_TZ" || m[1] == "TZ" && zone == "" {
				zone = strings.Trim(strings.TrimSpace(m[2]), `"'`)
			}

			continue
		}
		fields := strings.Fields(text)
		var schedule string
		var rest []string
		switch {
		case strings.HasPrefix(fields[0], "@"):
			if !cronNickname[strings.ToLower(fields[0])] || len(fields) < 3 {
				continue
			}
			schedule, rest = strings.ToLower(fields[0]), fields[1:]
		case len(fields) >= 7:
			ok := true
			for _, f := range fields[:5] {
				if !validCronField(f) {
					ok = false

					break
				}
			}
			if !ok {
				continue
			}
			schedule, rest = strings.Join(fields[:5], " "), fields[5:]
		default:
			continue
		}
		if !cronUser.MatchString(rest[0]) {
			continue
		}
		out = append(out, cronLine{
			source:   path,
			line:     n,
			schedule: schedule,
			user:     rest[0],
			command:  commandAfter(text, len(fields)-len(rest)+1),
			zone:     zone,
		})
	}

	return out
}

var cronNames = map[string]bool{
	"jan": true, "feb": true, "mar": true, "apr": true, "may": true, "jun": true, "jul": true, "aug": true, "sep": true, "oct": true, "nov": true, "dec": true,
	"sun": true, "mon": true, "tue": true, "wed": true, "thu": true, "fri": true, "sat": true,
}

// validCronField is whether cron would read one of the five time fields: lists of `*`, numbers and the names of
// months and days, as ranges, with steps.
func validCronField(f string) bool {
	for _, part := range strings.Split(strings.ToLower(f), ",") {
		rng, step, stepped := strings.Cut(part, "/")
		if stepped && !digits(step) {
			return false
		}
		if rng == "*" {
			continue
		}
		lo, hi, isRange := strings.Cut(rng, "-")
		if !atom(lo) || (isRange && !atom(hi)) {
			return false
		}
	}

	return true
}

func atom(s string) bool { return digits(s) || cronNames[s] }

func digits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}

// commandAfter is the line from its n-th field on, with its own spacing kept. Cron sends what follows an unescaped
// `%` to the command's input, so that is not part of the command.
func commandAfter(line string, n int) string {
	rest := line
	for i := 0; i < n; i++ {
		rest = strings.TrimLeft(rest, " \t")
		if j := strings.IndexAny(rest, " \t"); j >= 0 {
			rest = rest[j:]
		} else {
			rest = ""
		}
	}
	rest = strings.TrimSpace(rest)
	for i := 0; i < len(rest); i++ {
		if rest[i] == '\\' {
			i++

			continue
		}
		if rest[i] == '%' {
			return strings.TrimSpace(rest[:i])
		}
	}

	return rest
}

var runParts = regexp.MustCompile(`\brun-parts\b.*?/etc/cron\.(hourly|daily|weekly|monthly)\b`)

// runPartsFolder is the folder a line runs with run-parts, if it is one of those lines.
func runPartsFolder(command string) (string, bool) {
	if m := runParts.FindStringSubmatch(command); m != nil {
		return m[1], true
	}

	return "", false
}

var (
	// Debian's cron and run-parts both skip a file whose name has anything but these — `backup.sh` in /etc/cron.d
	// never runs there, so it is not listed there either.
	debianName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// What cronie and RHEL's run-parts skip: editor and package-manager leftovers.
	leftover = regexp.MustCompile(`(~|,|\.rpm(save|orig|new)|\.dpkg-[a-z]+|\.swp|\.cfsaved|\.bak)$`)
)

// cronFiles lists a folder's job files by the rules the machine's cron uses: no hidden files, no leftovers, and on
// Debian and Ubuntu only names cron itself would read. A run-parts folder runs only executable files.
func cronFiles(dir string, debian, executable bool) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || leftover.MatchString(name) || (debian && !debianName.MatchString(name)) {
			continue
		}
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() || (executable && fi.Mode().Perm()&0o111 == 0) {
			continue
		}
		out = append(out, name)
		if len(out) >= maxCronFiles {
			break
		}
	}
	sort.Strings(out)

	return out
}

// The words before the program that are not it: conditions, shell keywords, and the wrappers people put in front of
// a job. `[ -x /usr/lib/php/sessionclean ] && if [ ! -d /run/systemd/system ]; then /usr/lib/php/sessionclean; fi`
// runs sessionclean, not `[`.
var (
	shellWords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "fi": true, "do": true, "done": true, "!": true, "{": true, "}": true, "(": true, ")": true, "exec": true, "command": true, "time": true, "&&": true, "||": true, ";": true, "|": true}
	// A wrapper takes the next word as its program, after its own options.
	wrappers = map[string]bool{"nice": true, "ionice": true, "nohup": true, "chronic": true, "env": true, "flock": true, "timeout": true, "sudo": true, "su": true, "setlock": true, "cronic": true, "sh": false}
)

// Interpreters, named by the script they run.
var interpreters = map[string]bool{"php": true, "python": true, "python3": true, "perl": true, "ruby": true, "node": true, "bash": true, "sh": true}

// programOf is the program a command runs, by its file name, without anything that follows it.
func programOf(command string) string {
	words := shellSplit(command)
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case shellWords[w]:
			continue
		case w == "test" || w == "[" || w == "[[":
			// A condition, to the end of its expression.
			for i < len(words) && words[i] != "]" && words[i] != "]]" && words[i] != "&&" && words[i] != "||" && words[i] != ";" {
				i++
			}

			continue
		case w == "cd":
			i++

			continue
		case cronEnvLine.MatchString(w) && !strings.HasPrefix(w, "-"):
			continue // VAR=value
		case wrappers[filepath.Base(w)]:
			// Its own options, and for flock/timeout the one argument before the program.
			base := filepath.Base(w)
			for i+1 < len(words) && strings.HasPrefix(words[i+1], "-") {
				i++
				if (base == "ionice" || base == "nice" || base == "sudo" || base == "su") && len(words[i]) == 2 && i+1 < len(words) {
					i++ // -n 10, -c 3, -u www-data
				}
			}
			if (base == "flock" || base == "timeout" || base == "setlock") && i+1 < len(words) {
				i++
			}

			continue
		}
		if strings.HasPrefix(w, "-") || w == "" {
			continue
		}
		// An interpreter is named by the script it runs, when that is a file: `php /var/www/bin/report.php` is
		// report.php. A path's last part, never what follows it.
		if interpreters[filepath.Base(w)] {
			for k := i + 1; k < len(words); k++ {
				next := words[k]
				if strings.HasPrefix(next, "-") {
					continue
				}
				if strings.Contains(next, "/") || strings.Contains(next, ".") {
					return filepath.Base(next)
				}

				break
			}
		}

		return filepath.Base(w)
	}

	return ""
}

// wrappedJob reads `infranest-agent run --job NAME [--no-output] -- CMD …` out of a command: the job's name, and the
// program it runs. Empty when the line does not go through the wrapper.
func wrappedJob(command string) (name, program string) {
	words := shellSplit(command)
	for i := 0; i < len(words); i++ {
		if filepath.Base(words[i]) != "infranest-agent" || i+1 >= len(words) || words[i+1] != "run" {
			continue
		}
		for j := i + 2; j < len(words); j++ {
			switch {
			case words[j] == "--job" && j+1 < len(words):
				name = words[j+1]
				j++
			case strings.HasPrefix(words[j], "--job="):
				name = strings.TrimPrefix(words[j], "--job=")
			case words[j] == "--":
				if name != "" {
					return name, programOf(strings.Join(words[j+1:], " "))
				}

				return "", ""
			}
		}

		return "", ""
	}

	return "", ""
}

// shellSplit splits a command into words the way a shell would for reading it — quotes joined, operators apart.
// Not a shell: nothing is expanded, and a construct it does not know is left as a word.
func shellSplit(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			out = append(out, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' || c == '"':
			inWord = true
			end := strings.IndexByte(s[i+1:], c)
			if end < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
			} else {
				cur.WriteString(s[i+1 : i+1+end])
				i += end + 1
			}
		case c == '\\' && i+1 < len(s):
			inWord = true
			cur.WriteByte(s[i+1])
			i++
		case c == ' ' || c == '\t':
			flush()
		case c == ';' || c == '|' || c == '&' || c == '(' || c == ')':
			flush()
			op := string(c)
			if i+1 < len(s) && (c == '|' || c == '&') && s[i+1] == c {
				op += string(c)
				i++
			}
			out = append(out, op)
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	flush()

	return out
}

// systemZone is the machine's time zone by its IANA name, which is what cron reads the schedule in: the
// /etc/localtime link's target, else /etc/timezone. Empty when neither says.
func systemZone(root string) string {
	if target, err := os.Readlink(filepath.Join(root, "etc/localtime")); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}

	return strings.TrimSpace(readFile(filepath.Join(root, "etc/timezone")))
}

func readFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, maxCronFileBytes))

	return string(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
