package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/config"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/push"
)

// A push response must not be able to park this agent.
//
// `min_interval_seconds` went from the wire to a `select` timer with nothing in between, so one answer of
// 30000 stops this process for eight hours and it cannot recover — the correction only arrives on the next
// push, which is the thing that is no longer happening.
//
// Not a hostile-server story: `agent_min_interval` is a number an admin types into the plan builder, and a
// fleet-wide outage should not be one keystroke away from a cadence edit. The operator's own interval is
// already validated against a ceiling; the instruction arriving over the network was held to nothing.
func TestAnAbsurdServerCadenceIsRefusedRatherThanObeyed(t *testing.T) {
	log := &strings.Builder{}
	r := &Runner{
		Config: config.Config{Interval: time.Minute},
		Log:    log,
	}

	// The shape of a mistyped plan limit: 30000 seconds where 300 was meant.
	r.applyCadence(push.Result{MinInterval: 30000 * time.Second})

	if got := r.interval(); got != time.Minute {
		t.Fatalf("interval is %s — the agent adopted a cadence beyond the ceiling and has gone quiet", got)
	}
	if !strings.Contains(log.String(), "ceiling") {
		t.Fatalf("nothing said about the refusal; a misconfigured plan must be visible, not silent:\n%s", log)
	}
}

// A plausible plan cadence is still adopted — the ceiling must not break the feature it guards.
func TestAPlausibleServerCadenceIsStillAdopted(t *testing.T) {
	r := &Runner{Config: config.Config{Interval: time.Minute}, Log: &strings.Builder{}}

	r.applyCadence(push.Result{MinInterval: 5 * time.Minute})

	if got := r.interval(); got != 5*time.Minute {
		t.Fatalf("interval is %s, want 5m — the plan's cadence was not adopted", got)
	}
}

// Exactly at the ceiling is allowed: a bound nobody can reach is a different bound.
func TestTheCeilingItselfIsAllowed(t *testing.T) {
	r := &Runner{Config: config.Config{Interval: time.Minute}, Log: &strings.Builder{}}

	r.applyCadence(push.Result{MinInterval: maxServerInterval})

	if got := r.interval(); got != maxServerInterval {
		t.Fatalf("interval is %s, want %s", got, maxServerInterval)
	}
}

// A server asking for *more* frequent readings than configured is still ignored — the original rule.
func TestAServerCannotMakeTheAgentBusier(t *testing.T) {
	r := &Runner{Config: config.Config{Interval: 2 * time.Minute}, Log: &strings.Builder{}}

	r.applyCadence(push.Result{MinInterval: 10 * time.Second})

	if got := r.interval(); got != 2*time.Minute {
		t.Fatalf("interval is %s, want 2m — the operator asked for two minutes", got)
	}
}
