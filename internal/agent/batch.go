package agent

import "encoding/json"

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
// Order, not timestamps: the spool is keyed by a sequence number that increments per collection, so the
// last entry is the most recently collected. The server picks the newest by timestamp, and for a sender
// that appends in order the two agree — which ours does by construction.
//
// A sample that cannot be parsed is passed through untouched. This is an optimisation, and an
// optimisation that drops readings when it meets something unexpected is worse than the cost it saves.
func trimSnapshots(samples []json.RawMessage) []json.RawMessage {
	if len(samples) < 2 {
		return samples
	}

	out := make([]json.RawMessage, len(samples))
	copy(out, samples)

	for i := 0; i < len(out)-1; i++ {
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
