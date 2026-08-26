package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The token must never appear in a flare, in any form.
//
// This is the whole reason the command exists: the thing it replaces is "paste your config into the
// ticket", which puts a live credential into a chat thread, a screenshot and somebody's mail archive. A
// bundle that leaks it is worse than no bundle, because it is *asked for* and therefore always sent.
func TestAFlareNeverContainsTheToken(t *testing.T) {
	const secret = "sat_averysecrettokenvalue123456"

	env := map[string]string{
		"INFRANEST_TOKEN":     secret,
		"INFRANEST_URL":       "https://ingest.example.test",
		"INFRANEST_STATE_DIR": t.TempDir(),
	}

	var out bytes.Buffer
	if err := WriteFlare(&out, func(k string) string { return env[k] }, "v9.9.9", time.Unix(0, 0)); err != nil {
		t.Fatalf("flare failed: %v", err)
	}

	body := out.String()
	if strings.Contains(body, secret) {
		t.Fatal("the flare contains the token")
	}

	// Not a prefix either. `sat_avery…` confirms which token it is to anyone holding the list, and helps
	// diagnose nothing — a support bundle is pasted into places a credential should never reach.
	for _, n := range []int{8, 12, 16} {
		if strings.Contains(body, secret[:n]) {
			t.Fatalf("the flare contains the first %d characters of the token", n)
		}
	}
}

func TestAFlareSaysWhetherATokenIsPresentAndHowLong(t *testing.T) {
	// Presence and length are what actually diagnose things: a token truncated by a copy-paste is a real
	// failure and an invisible one, and neither fact is a secret in any useful sense.
	env := map[string]string{
		"INFRANEST_TOKEN":     "sat_abcdefghij",
		"INFRANEST_URL":       "https://ingest.example.test",
		"INFRANEST_STATE_DIR": t.TempDir(),
	}

	var out bytes.Buffer
	if err := WriteFlare(&out, func(k string) string { return env[k] }, "v9.9.9", time.Unix(0, 0)); err != nil {
		t.Fatalf("flare failed: %v", err)
	}

	var flare Flare
	if err := json.Unmarshal(out.Bytes(), &flare); err != nil {
		t.Fatalf("flare is not valid JSON: %v", err)
	}

	if !flare.Config.TokenPresent {
		t.Fatal("a configured token was reported as absent")
	}
	if flare.Config.TokenLength != len("sat_abcdefghij") {
		t.Fatalf("token length %d", flare.Config.TokenLength)
	}
	if flare.AgentVersion != "v9.9.9" {
		t.Fatalf("version %q", flare.AgentVersion)
	}
}

func TestAFlareIsGeneratedEvenWhenTheConfigIsBroken(t *testing.T) {
	// One of the reasons somebody runs this is that the agent will not start. A flare that refuses to
	// generate without a valid configuration fails exactly when it is needed.
	env := map[string]string{"INFRANEST_INTERVAL": "not-a-duration"}

	var out bytes.Buffer
	if err := WriteFlare(&out, func(k string) string { return env[k] }, "v9.9.9", time.Unix(0, 0)); err != nil {
		t.Fatalf("flare refused to generate: %v", err)
	}

	var flare Flare
	if err := json.Unmarshal(out.Bytes(), &flare); err != nil {
		t.Fatalf("flare is not valid JSON: %v", err)
	}

	if flare.ConfigError == "" {
		t.Fatal("a broken config was not reported")
	}
	if flare.Config.TokenPresent {
		t.Fatal("no token was set, but one was reported")
	}
}
