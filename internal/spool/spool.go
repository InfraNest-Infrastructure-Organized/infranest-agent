// Package spool keeps readings that could not be delivered, so a network problem costs nothing.
//
// This is the reason the server had to learn to accept a batch and a backfill. Without somewhere to put
// them, a reading that failed to send was simply lost — and the readings that fail to send are, by
// definition, the ones from the minutes something was wrong. A monitoring agent that drops exactly those
// has dropped the ones anybody will ask about afterwards.
package spool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Spool is a directory of one-reading files, oldest first by name.
//
// One file per reading rather than one appended log, because the operations that matter here are "add
// one" and "delete the ones that were accepted", and a log makes the second of those a rewrite. A partial
// write also costs one reading instead of truncating the file that holds all of them.
type Spool struct {
	dir string

	// Max is how many readings may be held. At a one-minute cadence the default is about eight hours of
	// outage, which is far longer than the server will accept a backfill for anyway — the cap is here so
	// a machine that can never reach us does not slowly fill its own disk. A monitoring agent must not
	// become the reason a server falls over; that is the one failure it is not allowed to have.
	Max int
}

const DefaultMax = 500

// Count is how many readings are waiting, and creates nothing.
//
// Separate from New because New *makes* the directory, and a read-only caller must not. `status` used
// New to get this number, so running `sudo infranest-agent status` created /var/lib/infranest-agent/spool
// owned by root — after which the agent, which runs unprivileged, could never write to it again. The
// command that exists to diagnose the agent permanently broke it, and then reported "nothing delivered
// yet, give it a minute" for ever, because a spool it cannot write is also a spool it cannot read.
//
// Found by an operator following our own instructions on a real server.
func Count(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Absent, or unreadable. Neither is a number, and neither is worth creating anything over.
		return 0
	}

	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}

	return n
}

func New(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the spool directory %s: %w", dir, err)
	}

	return &Spool{dir: dir, Max: DefaultMax}, nil
}

// Entry is one spooled reading: the bytes as they will be sent, and where they came from.
type Entry struct {
	Name string
	Data json.RawMessage
}

// Add writes one reading, evicting the oldest if the spool is full.
//
// Evicting the oldest rather than refusing the newest is the right way round: when the two collide, the
// recent past is what somebody is about to look at, and the reading from eight hours ago is the one the
// server would refuse as `too_old` anyway.
func (s *Spool) Add(seq int64, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("cannot encode the reading: %w", err)
	}

	// Zero-padded so lexical order is chronological order — which is what makes listing cheap and
	// removes any need to parse a filename to sort it.
	name := fmt.Sprintf("%020d.json", seq)
	tmp := filepath.Join(s.dir, name+".tmp")

	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("cannot write the reading: %w", err)
	}
	// Rename is what makes a reading appear all at once. A reader that catches a half-written file would
	// fail to parse it and drop a good reading for a reason that has nothing to do with the machine.
	if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("cannot commit the reading: %w", err)
	}

	return s.trim()
}

// Take returns up to n readings, oldest first, and the names to Drop once they are accepted.
//
// Nothing is deleted here. A reading is removed only after the server has said it stored it, so a push
// that fails, times out, or is answered by something that is not us leaves the spool exactly as it was.
func (s *Spool) Take(n int) ([]Entry, error) {
	names, err := s.names()
	if err != nil {
		return nil, err
	}
	if len(names) > n {
		names = names[:n]
	}

	entries := make([]Entry, 0, len(names))

	for _, name := range names {
		body, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("cannot read %s: %w", name, err)
		}
		if !json.Valid(body) {
			// Unreadable on disk is unreadable for ever; keeping it would block the spool head and stop
			// every later reading from being delivered. Dropping one damaged file is the smaller loss.
			_ = os.Remove(filepath.Join(s.dir, name))

			continue
		}
		entries = append(entries, Entry{Name: name, Data: body})
	}

	return entries, nil
}

// Drop removes readings the server has accepted.
func (s *Spool) Drop(entries []Entry) {
	for _, e := range entries {
		_ = os.Remove(filepath.Join(s.dir, e.Name))
	}
}

// Len is how many readings are waiting. Reported by `status`, because "nothing has been delivered for an
// hour and 60 readings are queued" is a different problem from "nothing has been collected".
func (s *Spool) Len() int {
	names, err := s.names()
	if err != nil {
		return 0
	}

	return len(names)
}

func (s *Spool) names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read the spool directory: %w", err)
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	return names, nil
}

func (s *Spool) trim() error {
	names, err := s.names()
	if err != nil {
		return err
	}
	for i := 0; i < len(names)-s.Max; i++ {
		_ = os.Remove(filepath.Join(s.dir, names[i]))
	}

	return nil
}
