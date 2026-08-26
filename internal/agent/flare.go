package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/collect"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/config"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/spool"
)

// Flare is everything a support conversation needs and nothing it does not (#792).
//
// The alternative it replaces is "please send us your config and the last bit of the journal", which asks
// somebody to paste a file containing a live credential into a ticket — and they will, because they want
// help and the file is what was asked for. This gathers the same picture with the credential removed
// before it is ever printed.
//
// It is a *diagnostic*, so it errs toward saying what it could not find rather than omitting it: a field
// reading "could not read /etc/infranest/agent.conf: permission denied" is the answer more often than the
// contents would have been.
type Flare struct {
	GeneratedAt time.Time `json:"generated_at"`

	AgentVersion string `json:"agent_version"`
	GoVersion    string `json:"go_version"`
	Platform     string `json:"platform"`

	Config FlareConfig `json:"config"`
	State  *State      `json:"state,omitempty"`

	// Readings written to disk and not yet delivered. The single most useful number when somebody says
	// "it stopped working on Tuesday": a spool that is growing means the agent is fine and the network or
	// the server is not.
	SpooledReadings int `json:"spooled_readings"`

	ConfigError string `json:"config_error,omitempty"`

	// What a collection cycle managed right now, and what it did not. Run live rather than remembered,
	// because "the disk collector has been failing since Tuesday" is exactly the thing nobody notices.
	CollectorErrors map[string]string `json:"collector_errors,omitempty"`
	CollectError    string            `json:"collect_error,omitempty"`
}

// FlareConfig is the configuration as the agent actually resolved it, with the credential removed.
//
// The token is never included in any form — not truncated, not hashed. A prefix is enough to confirm
// which token it is to anyone who already has the list, and a hash is a credential's fingerprint; neither
// helps diagnose anything, and a support bundle is pasted into tickets, chat threads and screenshots.
// What somebody actually needs to know is whether a token is *present*, which is a boolean.
type FlareConfig struct {
	TokenPresent bool   `json:"token_present"`
	TokenLength  int    `json:"token_length"`
	URL          string `json:"url"`
	Interval     string `json:"interval"`
	Processes    bool   `json:"processes"`
	ProcessArgs  bool   `json:"process_args"`
	Services     bool   `json:"services"`
	StateDir     string `json:"state_dir"`
}

// WriteFlare gathers a support bundle and writes it as indented JSON.
//
// `getenv` rather than the process environment directly, for the reason every other command here takes
// it: it is what makes this testable without setting variables in the test process.
func WriteFlare(w io.Writer, getenv func(string) string, version string, now time.Time) error {
	flare := Flare{
		GeneratedAt:  now.UTC(),
		AgentVersion: version,
		GoVersion:    runtime.Version(),
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Deliberately not fatal. A broken configuration is one of the reasons somebody runs this, so a flare
	// that refuses to generate without a valid config fails exactly when it is needed.
	cfg, err := config.Load(getenv)
	if err != nil {
		flare.ConfigError = err.Error()
	}

	flare.Config = FlareConfig{
		TokenPresent: strings.TrimSpace(cfg.Token) != "",
		// The length, because a token truncated by a copy-paste is a real and invisible failure — and a
		// length is not a secret in any useful sense.
		TokenLength: len(strings.TrimSpace(cfg.Token)),
		URL:         cfg.URL,
		Interval:    cfg.Interval.String(),
		Processes:   cfg.Processes,
		ProcessArgs: cfg.ProcessArgs,
		Services:    cfg.Services,
		StateDir:    cfg.StateDir,
	}

	// Both of these already answer "I could not read that" as a zero value rather than an error, which
	// is the right shape for a diagnostic: a missing state file is itself a finding, not a failure to
	// report one.
	state := LoadState(cfg.StateDir)
	flare.State = &state
	flare.SpooledReadings = spool.Count(cfg.StateDir + "/spool")

	// One real cycle, with processes off whatever the configuration says. A flare is pasted into a
	// ticket, and process names are the one thing in this payload that can identify what a customer runs.
	sample, err := collect.Collect(collect.Options{Services: cfg.Services, CPUInterval: 0})
	if err != nil {
		flare.CollectError = err.Error()
	} else if len(sample.Failed) > 0 {
		flare.CollectorErrors = sample.Failed
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(flare); err != nil {
		return fmt.Errorf("writing the flare: %w", err)
	}

	return nil
}
