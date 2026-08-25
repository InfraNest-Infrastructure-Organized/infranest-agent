// Package agent is the loop: collect, spool, send, and remember enough to answer "is it working?".
package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State is what `status` reads, and the only thing the run loop persists besides the spool.
//
// It exists because the first question anybody asks is "why is this server not reporting?", and the
// honest answer usually lives on the machine rather than in the platform: the token was deleted, the
// clock is wrong, the network is refusing. Making them run a command that reaches our API to find out why
// they cannot reach our API is not an answer.
type State struct {
	LastAttemptAt time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`

	// Positive means this machine's clock is ahead of ours. Recorded because a drifted clock is refused
	// by the server and looks, from every other angle, exactly like a machine that has gone quiet.
	ClockSkewSeconds float64 `json:"clock_skew_seconds,omitempty"`

	// Set when the token has been rejected. The loop stops pushing on this, so it is the difference
	// between "cannot reach InfraNest" and "InfraNest does not want to hear from this machine".
	TokenRejected   bool      `json:"token_rejected,omitempty"`
	TokenRejectedAt time.Time `json:"token_rejected_at,omitempty"`

	// What each collector last said, by name — empty when they all worked. The same map the server now
	// stores, kept here too because the machine is where somebody looks when a chart has a hole in it,
	// and because this half is readable when the network is not.
	CollectorErrors map[string]string `json:"collector_errors,omitempty"`

	// When the disk was last walked. Persisted so a restart does not re-walk immediately — an agent
	// restarting in a loop would otherwise scan every time it came up, which is the most expensive thing
	// it can do, on a machine already having a bad day.
	LastUsageScanAt time.Time `json:"last_usage_scan_at,omitempty"`

	// Where readings are being sent, after any redirect the server asked for. Persisted so a restart does
	// not undo a migration and quietly send the fleet back to the old host.
	URL string `json:"url,omitempty"`
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// LoadState never fails in a way the caller has to handle: a missing or damaged state file means we have
// simply forgotten, which costs a `status` line and nothing else.
func LoadState(dir string) State {
	var s State

	body, err := os.ReadFile(statePath(dir))
	if err != nil {
		return s
	}
	_ = json.Unmarshal(body, &s)

	return s
}

// SaveState writes atomically, for the same reason the spool does: a half-written state file read on the
// next start would look like corruption of something that is only a note to ourselves.
func SaveState(dir string, s State) error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, statePath(dir))
}
