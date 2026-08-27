package agent

import (
	"encoding/json"
	"time"
)

// snapshotFields are the parts of a reading that describe *now* rather than a moment.
//
// The server applies them from the newest sample in a batch and ignores the rest — deliberately, because
// a snapshot is current state: applying each sample's in turn would leave the disk card showing whichever
// happened to be last in the payload, an hour-old state presented as current.
//
// Mounts are absent from this list on purpose. They look like a snapshot and are also a *measurement*:
// `worst_mount_percent` is derived per sample and is what a disk rule averages over its window, so a
// backfilled sample without mounts is a hole in that series rather than a saving.
var snapshotFields = []string{"services", "processes", "system"}

// trimSnapshots strips the snapshot fields from every sample except the newest.
//
// A backfill carries up to MaxBatch readings and, until this, each one carried the full service list — on
// an ordinary server that is 68 units repeated sixty times, of which the server keeps one copy. Measured
// at the caps the published contract allows, a batch went from 32.4 MB to 5.6 MB, which is the difference
// between exceeding the deployed request limit and fitting inside it with room.
//
// It matters most exactly when it is worst: a batch is only large because the agent has been unable to
// send, which means the network is already the problem.
//
// By timestamp, because that is what the server picks by.
//
// This used to keep the last sample by *position*, on the grounds that the spool appends in sequence
// order so position and time agree "by construction". They agree only while the clock moves forwards. An
// NTP correction or a resumed VM can stamp a later collection with an earlier time, and then the two ends
// disagree about which sample is newest: this strips the snapshot from the one the server is about to
// read it from, and that batch silently updates no services, processes or system facts at all. It
// self-heals on the next push, which is precisely why nobody would ever catch it.
//
// A sample whose timestamp cannot be read falls back to its position, and a sample that cannot be parsed
// at all is passed through untouched. This is an optimisation, and an optimisation that drops readings
// when it meets something unexpected is worse than the cost it saves.
func trimSnapshots(samples []json.RawMessage) []json.RawMessage {
	if len(samples) < 2 {
		return samples
	}

	out := make([]json.RawMessage, len(samples))
	copy(out, samples)

	keep := newestIndex(out)

	for i := 0; i < len(out); i++ {
		if i == keep {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(out[i], &fields); err != nil {
			continue
		}

		trimmed := false
		for _, key := range snapshotFields {
			if _, ok := fields[key]; ok {
				delete(fields, key)
				trimmed = true
			}
		}
		if !trimmed {
			continue
		}

		if encoded, err := json.Marshal(fields); err == nil {
			out[i] = encoded
		}
	}

	return out
}

// newestIndex is the sample the server will treat as current: the greatest `collected_at`.
//
// Ties and unreadable timestamps both fall back to the later position, which is what this used to do for
// every sample. That keeps the old behaviour as the floor rather than the rule — a batch the clock never
// disturbed trims exactly as before.
func newestIndex(samples []json.RawMessage) int {
	keep := len(samples) - 1

	var best time.Time
	found := false

	for i, raw := range samples {
		var fields struct {
			CollectedAt time.Time `json:"collected_at"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil || fields.CollectedAt.IsZero() {
			continue
		}
		if !found || !fields.CollectedAt.Before(best) {
			best, keep, found = fields.CollectedAt, i, true
		}
	}

	return keep
}
