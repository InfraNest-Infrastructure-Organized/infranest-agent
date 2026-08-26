package config

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultConfigPath is where the installers write the agent's settings.
//
// A file, and not only the environment, because the two platforms disagree about what a service
// environment is. systemd has EnvironmentFile and loads this for us; the Windows Task Scheduler has no
// equivalent, so a task started at boot inherits nothing — which meant the Windows agent was installed
// with a token it had no way of reading. The alternatives were worse in the same direction: a
// machine-wide environment variable puts the token in every process on the box, and a task argument puts
// it in the Task Scheduler UI and in the process list.
func DefaultConfigPath(getenv func(string) string) string {
	if dir := getenv("PROGRAMDATA"); dir != "" {
		return filepath.Join(dir, "InfraNest", "agent.conf")
	}

	return "/etc/infranest/agent.conf"
}

// readFile parses the KEY=VALUE file the installers write.
//
// Deliberately a small subset of systemd's EnvironmentFile syntax — comments, blank lines, and one
// unquoted value per line — because that is exactly what our installers produce. Supporting more would
// mean this parser and systemd's could disagree about the same file on the same machine, and the values
// here are a token, a URL and a duration: none of them wants a shell.
func readFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// One level of quoting, because a copied-and-pasted line often carries it and a token wrapped in
		// quotes would otherwise be sent with them attached — a 401 whose cause is invisible.
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		if key != "" {
			values[key] = value
		}
	}

	return values, scanner.Err()
}

// withFileFallback returns a getenv that falls back to the config file.
//
// The environment wins. On Linux systemd has already loaded the same file into it, so the two agree; when
// they do not, it is because somebody set a variable deliberately — in a container, in a unit override,
// on the command line — and that intent should not be overruled by a file they may not know exists.
func withFileFallback(getenv func(string) string) func(string) string {
	path := strings.TrimSpace(getenv("INFRANEST_CONFIG"))
	if path == "" {
		path = DefaultConfigPath(getenv)
	}

	values, err := readFile(path)

	// A file that is *there* and unreadable is a different problem from one that is absent, and the
	// difference is the whole answer. Somebody who has just run the installer — which printed "wrote
	// /etc/infranest/agent.conf" — and then runs `status` as themselves gets told the token is not set,
	// which contradicts what they just watched happen and points at the wrong fix. The file is 0600 and
	// owned by the agent user, on purpose; the answer is `sudo`, and nothing was saying so.
	unreadable := ""
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		unreadable = path
	}

	return func(key string) string {
		if key == unreadableKey {
			return unreadable
		}
		if v := getenv(key); v != "" {
			return v
		}

		return values[key]
	}
}

// unreadableKey is how withFileFallback reports a config file it could not open, through the same
// getenv-shaped channel everything else here travels down. Not a real environment variable — the leading
// space cannot appear in one, so nothing on the machine can collide with it.
const unreadableKey = " config-unreadable"
