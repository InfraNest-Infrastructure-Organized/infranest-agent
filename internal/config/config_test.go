package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestATokenIsRequired(t *testing.T) {
	// An agent with no credential can only fail, and failing at startup is the loud version of that.
	// The quiet version — start, look healthy, send nowhere — is indistinguishable from a dead server.
	if _, err := Load(env(nil)); err == nil {
		t.Fatal("expected a missing token to be refused")
	}
}

func TestPlaintextIsRefused(t *testing.T) {
	// The token is a bearer credential. Sending it over http once is enough to lose it, and there is no
	// configuration for which that is the right trade — not even a local test.
	_, err := Load(env(map[string]string{"INFRANEST_TOKEN": "sat_x", "INFRANEST_URL": "http://ingest.example.com"}))
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("expected http to be refused, got %v", err)
	}
}

func TestCredentialsInTheURLAreRefused(t *testing.T) {
	_, err := Load(env(map[string]string{"INFRANEST_TOKEN": "sat_x", "INFRANEST_URL": "https://user:pass@ingest.example.com"}))
	if err == nil {
		t.Fatal("expected a URL carrying credentials to be refused")
	}
}

func TestTheIntervalIsBounded(t *testing.T) {
	for _, raw := range []string{"1s", "30m", "banana"} {
		if _, err := Load(env(map[string]string{"INFRANEST_TOKEN": "sat_x", "INFRANEST_INTERVAL": raw})); err == nil {
			t.Fatalf("expected %q to be refused", raw)
		}
	}

	c, err := Load(env(map[string]string{"INFRANEST_TOKEN": "sat_x", "INFRANEST_INTERVAL": "30s"}))
	if err != nil || c.Interval != 30*time.Second {
		t.Fatalf("expected 30s to be accepted, got %v %v", c.Interval, err)
	}
}

func TestCommandLinesCannotBeTurnedOnByAccident(t *testing.T) {
	// The generous reading of "args on, processes off" is to turn processes on too — which would ship a
	// customer's command lines, and their credentials, from an operator who set one variable.
	_, err := Load(env(map[string]string{"INFRANEST_TOKEN": "sat_x", "INFRANEST_PROCESS_ARGS": "true"}))
	if err == nil {
		t.Fatal("expected process args without processes to be refused")
	}
}

func TestThePushPathIsDerivedNotConfigured(t *testing.T) {
	// So a base URL cannot be set to something almost right. A trailing slash is the classic version of
	// that, and it produces a 404 that looks like the server being down.
	for _, base := range []string{"https://ingest.example.com", "https://ingest.example.com/"} {
		c := Config{URL: base}
		if got := c.PushURL(); got != "https://ingest.example.com/api/metrics/push" {
			t.Fatalf("%s → %s", base, got)
		}
	}
}

func TestARedirectIsTakenOnlyWithinTheSameDomain(t *testing.T) {
	const current = "https://ingest.infranest.app/api/metrics/push"

	// The legitimate case: we move the fleet between our own hosts without anyone visiting a machine.
	if got, ok := Adopt(current, "https://ingest-eu.infranest.app/api/metrics/push"); !ok || !strings.Contains(got, "ingest-eu") {
		t.Fatalf("expected a same-domain move to be adopted, got %q %v", got, ok)
	}

	// The case this rule exists for. Anyone able to answer one push can already read it; being able to
	// keep the fleet for good is a different and permanent thing.
	for _, hostile := range []string{
		"https://ingest.infranest.app.evil.com/api/metrics/push",
		"https://evil.com/api/metrics/push",
		"http://ingest.infranest.app/api/metrics/push",
		"not a url at all",
	} {
		if got, ok := Adopt(current, hostile); ok || got != current {
			t.Fatalf("expected %q to be refused, got %q %v", hostile, got, ok)
		}
	}
}

func TestTheConfigFileIsReadWhenTheEnvironmentIsEmpty(t *testing.T) {
	// The bug this exists for is Windows-shaped. systemd loads /etc/infranest/agent.conf into the
	// environment for us; the Task Scheduler has no equivalent, so a task started at boot inherits
	// nothing — and the Windows agent was being installed with a token it had no way of reading.
	dir := t.TempDir()
	path := dir + "/agent.conf"
	if err := os.WriteFile(path, []byte(
		"# InfraNest agent configuration.\n\nINFRANEST_TOKEN=sat_from_file\nINFRANEST_INTERVAL=45s\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(env(map[string]string{"INFRANEST_CONFIG": path}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "sat_from_file" {
		t.Fatalf("token not read from the file: %q", c.Token)
	}
	if c.Interval != 45*time.Second {
		t.Fatalf("interval not read from the file: %v", c.Interval)
	}
}

func TestTheEnvironmentBeatsTheFile(t *testing.T) {
	// When the two disagree it is because somebody set a variable deliberately — in a container, a unit
	// override, a shell — and that intent should not be overruled by a file they may not know exists.
	dir := t.TempDir()
	path := dir + "/agent.conf"
	_ = os.WriteFile(path, []byte("INFRANEST_TOKEN=sat_from_file\n"), 0o600)

	c, err := Load(env(map[string]string{"INFRANEST_CONFIG": path, "INFRANEST_TOKEN": "sat_from_env"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "sat_from_env" {
		t.Fatalf("the file overruled the environment: %q", c.Token)
	}
}

func TestAQuotedValueLosesItsQuotes(t *testing.T) {
	// A copied-and-pasted line often carries them, and a token sent with quotes attached is a 401 whose
	// cause is invisible from either end.
	dir := t.TempDir()
	path := dir + "/agent.conf"
	_ = os.WriteFile(path, []byte("INFRANEST_TOKEN=\"sat_quoted\"\n"), 0o600)

	c, err := Load(env(map[string]string{"INFRANEST_CONFIG": path}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "sat_quoted" {
		t.Fatalf("quotes were not stripped: %q", c.Token)
	}
}

func TestAMissingConfigFileIsNotAnError(t *testing.T) {
	// The useful message is "INFRANEST_TOKEN is not set", not a permissions error about a path the
	// reader has never heard of.
	_, err := Load(env(map[string]string{"INFRANEST_CONFIG": "/nonexistent/agent.conf"}))
	if err == nil || !strings.Contains(err.Error(), "INFRANEST_TOKEN") {
		t.Fatalf("expected the missing token to be the complaint, got %v", err)
	}
}
