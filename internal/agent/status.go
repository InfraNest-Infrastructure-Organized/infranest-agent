package agent

import (
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/config"
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/spool"
)

// Status writes a plain answer to "is this working, and if not, why not?".
//
// Answered entirely from what is on this machine — no network call, no token needed beyond the one
// already configured. That is the point: the question is usually asked *because* the machine cannot reach
// us, and a diagnostic that has to reach us to run is no use in the one case it exists for.
//
// It is also written to be pasted into a support ticket, so it names the file to look at and never prints
// the token.
func Status(w io.Writer, cfg config.Config, now time.Time) {
	state := LoadState(cfg.StateDir)

	url := cfg.PushURL()
	if state.URL != "" {
		url = state.URL
	}

	fmt.Fprintf(w, "Sending to:   %s\n", url)
	fmt.Fprintf(w, "Every:        %s\n", cfg.Interval)
	fmt.Fprintf(w, "State in:     %s\n", cfg.StateDir)

	queued := 0
	if s, err := spool.New(cfg.StateDir + "/spool"); err == nil {
		queued = s.Len()
	}
	fmt.Fprintf(w, "Waiting:      %d reading(s) not yet delivered\n", queued)

	fmt.Fprintln(w)

	switch {
	case state.TokenRejected:
		// Named first and named plainly, because this is the one failure with a cause nobody guesses:
		// the machine is fine, the network is fine, and somebody deleted the token in InfraNest — which
		// is exactly how the agent is meant to be turned off from the other end.
		fmt.Fprintf(w, "NOT SENDING — InfraNest rejected this token%s.\n", since(state.TokenRejectedAt, now))
		fmt.Fprintln(w, "  The token was most likely deleted on the server's Metrics tab in InfraNest.")
		fmt.Fprintln(w, "  Create a new one there and re-run the installer with it, or remove this agent.")
	case state.LastSuccessAt.IsZero() && state.LastError != "":
		fmt.Fprintln(w, "NOT SENDING — no reading has ever been delivered.")
		fmt.Fprintf(w, "  Last error: %s\n", state.LastError)
	case state.LastSuccessAt.IsZero():
		fmt.Fprintln(w, "Nothing delivered yet. If the agent has just been installed, give it a minute.")
	case state.LastError != "":
		fmt.Fprintf(w, "FAILING — last delivery succeeded%s, but the most recent attempt did not.\n", since(state.LastSuccessAt, now))
		fmt.Fprintf(w, "  Last error: %s\n", state.LastError)
		fmt.Fprintln(w, "  Readings are being kept and will be sent when this clears.")
	default:
		fmt.Fprintf(w, "OK — last delivery succeeded%s.\n", since(state.LastSuccessAt, now))
	}

	if len(state.CollectorErrors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "COLLECTORS — some readings are not being taken:")
		// Sorted, so two runs of this command can be compared by eye or by diff in a ticket.
		names := make([]string, 0, len(state.CollectorErrors))
		for name := range state.CollectorErrors {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(w, "  %s: %s\n", name, state.CollectorErrors[name])
		}
		fmt.Fprintln(w, "  Everything else is still being collected and sent.")
	}

	// Reported whenever it is large enough to matter, not only when something has already failed: the
	// server refuses a reading stamped ahead of its own clock, and the symptom of that — on every other
	// surface — is a server that has simply gone quiet.
	if skew := math.Abs(state.ClockSkewSeconds); skew >= 60 {
		fmt.Fprintln(w)
		direction := "ahead of"
		if state.ClockSkewSeconds < 0 {
			direction = "behind"
		}
		fmt.Fprintf(w, "CLOCK — this machine is about %s %s InfraNest.\n",
			(time.Duration(skew) * time.Second).String(), direction)
		fmt.Fprintln(w, "  Readings stamped ahead of InfraNest's clock are refused, which looks exactly")
		fmt.Fprintln(w, "  like a server that has gone quiet. Enable NTP (timedatectl set-ntp true).")
	}
}

func since(t time.Time, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t).Round(time.Second)
	if d < 0 {
		d = 0
	}

	return fmt.Sprintf(" %s ago", d)
}
