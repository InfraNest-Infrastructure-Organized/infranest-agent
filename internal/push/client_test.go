package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func send(t *testing.T, handler http.HandlerFunc, samples ...string) (Result, error) {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	raw := make([]json.RawMessage, 0, len(samples))
	for _, s := range samples {
		raw = append(raw, json.RawMessage(s))
	}
	if len(raw) == 0 {
		raw = append(raw, json.RawMessage(`{"cpu_percent":1}`))
	}

	c := New("sat_test", "1.0.0")
	c.HTTP = srv.Client()
	c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return c.Send(context.Background(), srv.URL, raw, nil)
}

func TestTheBodyIsAlwaysABatch(t *testing.T) {
	// One shape on the wire. The flat form exists only for the install snippets, and the agent should
	// never be the thing that keeps a second shape alive.
	var got struct {
		Samples      []json.RawMessage `json:"samples"`
		AgentVersion string            `json:"agent_version"`
	}

	_, err := send(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if r.Header.Get("Authorization") != "Bearer sat_test" {
			t.Errorf("token not sent: %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"accepted":2}`))
	}, `{"cpu_percent":1}`, `{"cpu_percent":2}`)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Samples) != 2 {
		t.Fatalf("expected 2 samples in the envelope, got %d", len(got.Samples))
	}
	// So a fleet running a version with a known bug is visible rather than something to be discovered.
	if got.AgentVersion != "1.0.0" {
		t.Fatalf("agent version not sent: %q", got.AgentVersion)
	}
}

func TestARejectedTokenIsItsOwnKindOfFailure(t *testing.T) {
	// The correct response is the opposite of every other failure: a network error should be retried, and
	// this must not be.
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		_, err := send(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"message":"Invalid server agent token."}`))
		})
		if !errors.Is(err, ErrTokenRejected) {
			t.Fatalf("%d did not report a rejected token: %v", code, err)
		}
	}
}

func TestAPushThatStoredNothingIsAFailure(t *testing.T) {
	// A 201 would tell the agent its spool was delivered, and it would delete readings that were never
	// stored. This is the wire half of that promise.
	_, err := send(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"No reading in this push could be accepted.","accepted":0}`))
	})
	if !errors.Is(err, ErrServerRefused) {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

func TestARedirectIsNotFollowed(t *testing.T) {
	// A 30x is how a bearer credential ends up somewhere it was never meant to go. The legitimate way to
	// move an agent is the `ingest_url` field, which is checked against the current domain first.
	var reached string

	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = "the redirect target"
		w.WriteHeader(http.StatusCreated)
	}))
	defer elsewhere.Close()

	_, err := send(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, elsewhere.URL, http.StatusTemporaryRedirect)
	})

	if reached != "" {
		t.Fatal("the client followed a redirect and sent the token to another host")
	}
	if err == nil {
		t.Fatal("expected an unfollowed redirect to be reported as a failure")
	}
}

func TestARefusalIsReportedRatherThanSwallowed(t *testing.T) {
	// "Your clock is eight minutes fast" is the answer to a question that otherwise has none.
	result, err := send(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"accepted":1,"skipped":[{"reason":"ahead_of_server_time","collected_at":"2026-08-25T12:00:00Z"}],"server_time":"2026-08-25T11:52:00Z"}`))
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != "ahead_of_server_time" {
		t.Fatalf("refusal not surfaced: %+v", result.Skipped)
	}
	if result.ServerTime.IsZero() {
		t.Fatal("the server clock was not read")
	}
}

func TestAnUnparseableBodyIsNotFatalOnItsOwn(t *testing.T) {
	// A proxy serving an HTML error page should look like the 502 it is, not like a parsing bug.
	_, err := send(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>Bad Gateway</html>"))
	})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("expected the status code to be the answer, got %v", err)
	}
}
