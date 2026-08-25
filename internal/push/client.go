// Package push delivers readings to InfraNest.
//
// It only ever sends. The response is read for three things — how many readings were stored, what the
// server's clock says, and where to send next — and none of them can make this process do anything but
// send to a different address within the same domain. There is no field here that runs, writes or
// executes anything, and there is deliberately no way to add one without it being obvious in review.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Result is what one push accomplished.
type Result struct {
	Accepted   int
	Skipped    []Skipped
	ServerTime time.Time
	IngestURL  string

	// Settled means the server gave a per-reading verdict on this batch — some stored, some refused for
	// a reason that will not change. Those readings are done with either way and must leave the spool.
	//
	// Without this the spool could wedge permanently, and the case is ordinary rather than exotic: a
	// push lands, its response is lost, the agent retries the batch it cannot know arrived, and every
	// reading in it comes back `already_covered`. Treating that as a failure means keeping readings the
	// server already holds and offering them again for ever — the head of the spool never clears and
	// nothing behind it is ever sent.
	Settled bool
}

// Skipped is one reading the server would not store, and why. Surfaced rather than swallowed: "your
// clock is eight minutes fast" is the answer to a question that otherwise has no answer anywhere.
type Skipped struct {
	Reason      string    `json:"reason"`
	CollectedAt time.Time `json:"collected_at"`
}

// ErrTokenRejected means the token is no longer valid — almost always because it was deleted in
// InfraNest, which is how somebody uninstalls the agent from the other end.
//
// Distinguished from every other failure because the correct response is the opposite one: a network
// error should be retried for ever, and this must not be. Retrying a rejected credential once a minute
// for months writes an authentication failure into somebody's logs on a schedule, and the reading it is
// carrying will never be wanted by anyone.
var ErrTokenRejected = errors.New("the server rejected this token")

// ErrServerRefused means the server understood the request and stored none of it — the spool must be
// kept, because these readings have not been delivered.
var ErrServerRefused = errors.New("the server accepted no readings from this push")

type Client struct {
	HTTP    *http.Client
	Token   string
	Version string
}

func New(token, version string) *Client {
	return &Client{
		// An explicit client, never http.DefaultClient: the default has no timeout at all, so a
		// connection that is accepted and then never answered would hang this agent for ever. A
		// monitoring agent that silently stops is the failure mode with no symptom.
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			// Redirects are refused rather than followed. A 30x is how a credential ends up somewhere
			// it was not meant to go, and the legitimate way to move an agent is the `ingest_url` in a
			// push response — which is checked against the current domain before it is taken.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		Token:   token,
		Version: version,
	}
}

// Send posts a batch of readings and reports what the server did with them.
func (c *Client) Send(ctx context.Context, url string, samples []json.RawMessage, failed map[string]string, usage any) (Result, error) {
	var result Result

	body, err := encode(samples, failed, c.Version, usage)
	if err != nil {
		return result, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("cannot build the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "infranest-agent/"+c.Version)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return result, fmt.Errorf("cannot reach InfraNest: %w", err)
	}
	defer resp.Body.Close()

	// Bounded, because the body is parsed and this is the one place a machine we do not control talks
	// back to us. A response that is somehow enormous must cost a failed push, not this process.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return result, fmt.Errorf("cannot read the response: %w", err)
	}

	var decoded struct {
		Message    string    `json:"message"`
		Accepted   int       `json:"accepted"`
		Skipped    []Skipped `json:"skipped"`
		ServerTime time.Time `json:"server_time"`
		IngestURL  string    `json:"ingest_url"`
	}
	// A body we cannot parse is not fatal on its own — the status code is the real answer, and a proxy
	// serving an HTML error page should not look different from the 502 it is.
	_ = json.Unmarshal(payload, &decoded)

	result = Result{
		Accepted:   decoded.Accepted,
		Skipped:    decoded.Skipped,
		ServerTime: decoded.ServerTime,
		IngestURL:  decoded.IngestURL,
		// A verdict on the readings themselves, as opposed to a refusal of the request. Every reason the
		// server can give — already stored, too old, ahead of its clock — is terminal: none of them comes
		// good by being sent again, and holding one blocks everything behind it.
		Settled: decoded.Accepted > 0 || len(decoded.Skipped) > 0,
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return result, fmt.Errorf("%w: %s", ErrTokenRejected, firstLine(decoded.Message))
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return result, nil
	case resp.StatusCode == http.StatusUnprocessableEntity:
		return result, fmt.Errorf("%w: %s", ErrServerRefused, firstLine(decoded.Message))
	default:
		return result, fmt.Errorf("InfraNest answered %d: %s", resp.StatusCode, firstLine(decoded.Message))
	}
}

// encode builds the request body: a batch envelope, always.
//
// `samples` are already-marshalled readings straight from the spool, so a reading is written once when it
// is collected and never re-encoded — which is also what keeps a reading that was collected by an older
// version of the agent sendable by a newer one.
func encode(samples []json.RawMessage, failed map[string]string, version string, usage any) ([]byte, error) {
	payload := struct {
		Samples      []json.RawMessage `json:"samples"`
		Failed       map[string]string `json:"failed,omitempty"`
		AgentVersion string            `json:"agent_version,omitempty"`
		// A directory listing rides along on whichever push follows a scan, rather than having a delivery
		// path of its own. The sending half already has retry, backoff and a spool; a second one for a
		// single payload would be a second thing to get wrong. Omitted entirely on the pushes between
		// scans, which is nearly all of them.
		DiskUsage any `json:"disk_usage,omitempty"`
	}{Samples: samples, Failed: failed, AgentVersion: version, DiskUsage: usage}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("cannot encode the batch: %w", err)
	}

	return body, nil
}

// firstLine keeps a server message to one line for a log.
//
// The message comes from us, not from the machine, but it crosses a network and lands in journald — and a
// newline in a log line forges a second entry. The same reasoning applies here as on the receiving side.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(no message)"
	}

	return s
}
