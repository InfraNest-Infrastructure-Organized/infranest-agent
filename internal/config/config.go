// Package config loads the agent's settings from its environment.
//
// There is deliberately no config file parser here. systemd's EnvironmentFile already reads
// /etc/infranest/agent.conf into the process environment, Windows reads the same shape from the scheduled
// task, and a container passes plain environment variables. Adding a parser of our own would be a third
// way to express the same three settings, and the one most likely to disagree with the other two.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// DefaultURL is where readings go unless told otherwise.
//
// A name we intend never to change. DNS is the first and cheapest layer of indirection: this can be
// re-pointed at any host, region or provider without touching a single machine that has the agent
// installed, and re-pointing it is not a deploy. The second layer is the `ingest_url` a push response may
// name — see Config.Adopt — which exists for the case where the *name itself* has to change. Between the
// two, an installed agent should never need a human to visit the machine.
const DefaultURL = "https://ingest.infranest.app"

// Bounds on the collection interval. Below the floor an agent is a load generator rather than a monitor —
// and the server's own silence rule cannot mean anything faster than a minute anyway. Above the ceiling
// the readings are too sparse for a duration-averaged rule to be honest about.
const (
	MinInterval = 10 * time.Second
	MaxInterval = 5 * time.Minute
)

type Config struct {
	Token    string
	URL      string
	Interval time.Duration

	// Processes and ProcessArgs default to off. Process command lines routinely contain credentials, so
	// the default must not be to ship a customer's secrets to us; ProcessArgs must be asked for twice,
	// in the sense that it is a separate setting from collecting processes at all.
	Processes   bool
	ProcessArgs bool

	// Services watches systemd units (#774) and defaults to **on**, which is the opposite of the two
	// above and for the opposite reason: it carries no command lines and therefore no credentials, and
	// the failure it catches moves no metric at all. A machine whose backup timer died looks perfectly
	// healthy without it. `INFRANEST_SERVICES=0` turns it off for anyone who would rather it did not
	// talk to the system bus.
	Services bool

	// StateDir holds the spool and the last-run record. Everything in it is disposable: losing it costs
	// undelivered readings and nothing else.
	StateDir string
}

// Load reads the configuration from the environment, and refuses anything it cannot make sense of.
//
// Refusing loudly at startup is deliberate. The failure this prevents is an agent that starts, looks
// healthy in `systemctl status`, and sends nothing anywhere — which from the server's side is
// indistinguishable from a machine that has gone down.
func Load(getenv func(string) string) (Config, error) {
	// The environment first, the installed config file second — see file.go for why a file is read at
	// all when systemd already loads one for us.
	getenv = withFileFallback(getenv)

	c := Config{
		Token:     strings.TrimSpace(getenv("INFRANEST_TOKEN")),
		URL:       strings.TrimSpace(getenv("INFRANEST_URL")),
		StateDir:  strings.TrimSpace(getenv("INFRANEST_STATE_DIR")),
		Interval:  time.Minute,
		Processes: truthy(getenv("INFRANEST_PROCESSES")),
		// Default-on, so absence means yes and only an explicit falsey value turns it off.
		Services:    !falsey(getenv("INFRANEST_SERVICES")),
		ProcessArgs: truthy(getenv("INFRANEST_PROCESS_ARGS")),
	}

	if c.Token == "" {
		return c, errors.New("INFRANEST_TOKEN is not set — the agent has nothing to authenticate with")
	}
	if c.URL == "" {
		c.URL = DefaultURL
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir(getenv)
	}

	if raw := strings.TrimSpace(getenv("INFRANEST_INTERVAL")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return c, fmt.Errorf("INFRANEST_INTERVAL %q is not a duration (try 60s or 2m): %w", raw, err)
		}
		if d < MinInterval || d > MaxInterval {
			return c, fmt.Errorf("INFRANEST_INTERVAL %s is outside %s–%s", d, MinInterval, MaxInterval)
		}
		c.Interval = d
	}

	if err := ValidateURL(c.URL); err != nil {
		return c, err
	}

	// Asking for command lines without asking for processes is a contradiction, and the generous reading
	// of it — turn processes on too — would ship command lines from an operator who only set one variable.
	// The strict reading is the safe one.
	if c.ProcessArgs && !c.Processes {
		return c, errors.New("INFRANEST_PROCESS_ARGS is on but INFRANEST_PROCESSES is not — " +
			"set both if you really want command lines, which often contain credentials")
	}

	return c, nil
}

// ValidateURL refuses a destination the agent must not send a credential to.
//
// HTTPS only, and no user info in the URL. The token is a bearer credential written into a file on the
// customer's machine; sending it over plaintext HTTP once is enough to lose it, and there is no
// configuration for which that is the right trade.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("INFRANEST_URL %q is not a URL: %w", raw, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("INFRANEST_URL %q must be https — the token is a credential and this is the wire it crosses", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("INFRANEST_URL %q has no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("INFRANEST_URL %q must not carry a username or password", raw)
	}

	return nil
}

// Adopt reports whether the agent should start sending to the URL the server just named.
//
// The server can move a fleet without anyone visiting a machine, which is the whole reason this exists.
// What it must not become is a permanent redirect handed out by whoever manages to answer one push — so
// a new destination is taken only when it is HTTPS and shares the current registrable domain. An attacker
// who can answer for `ingest.infranest.app` can already read that push; being able to move the agent to
// `ingest-eu.infranest.app` gains them nothing they did not already have. Being able to move it to a host
// they own would be a different thing entirely, and permanent.
func Adopt(current, offered string) (string, bool) {
	if offered == "" || offered == current {
		return current, false
	}
	if err := ValidateURL(offered); err != nil {
		return current, false
	}

	cur, err1 := url.Parse(current)
	next, err2 := url.Parse(offered)
	if err1 != nil || err2 != nil {
		return current, false
	}

	if registrable(cur.Hostname()) != registrable(next.Hostname()) {
		return current, false
	}

	return offered, true
}

// registrable reduces a hostname to the last two labels.
//
// Deliberately naive, and safe in the direction that matters: it is used only to *refuse* a redirect, so
// being too strict costs a migration that has to be done another way, while being too loose would let a
// fleet be pointed somewhere else for good. A public-suffix list would make this exactly right and would
// mean vendoring a list that changes, in a binary whose whole claim is that it has no dependencies.
func registrable(host string) string {
	labels := strings.Split(strings.ToLower(strings.TrimSuffix(host, ".")), ".")
	if len(labels) < 2 {
		return strings.ToLower(host)
	}

	return strings.Join(labels[len(labels)-2:], ".")
}

// PushURL is the endpoint a reading is posted to.
//
// Derived rather than configured, so a base URL cannot be set to something that is *almost* right. A
// trailing slash, a path, or a bare host all end up at the same place.
func (c Config) PushURL() string {
	return strings.TrimRight(c.URL, "/") + "/api/metrics/push"
}

// falsey is truthy's opposite for a default-on setting: only an explicit no counts, so an unset variable
// and a misspelled one both leave the collector running rather than silently disabling it.
func falsey(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return true
	default:
		return false
	}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}

	return false
}

func defaultStateDir(getenv func(string) string) string {
	// Windows has no /var. PROGRAMDATA is where a service's own data belongs, and it is the same
	// directory the PowerShell installer writes the configuration into.
	if dir := getenv("PROGRAMDATA"); dir != "" {
		return dir + `\InfraNest\agent`
	}

	return "/var/lib/infranest-agent"
}
