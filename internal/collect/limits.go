package collect

import "unicode/utf8"

/*
What the ingest endpoint will accept, and the reason the agent enforces it rather than finding out.

A string longer than the server's limit does not cost that field — it fails validation for the whole
push, and a validation failure carries no per-reading verdict. So the batch is not "settled", stays at the
head of the spool, and is offered again for ever, with every later reading queued behind it. One
over-long value is therefore a permanent monitoring outage, on a machine nobody would think to look at.

The realistic trigger is `--process-args`: the kernel bounds `comm` to fifteen characters, but
/proc/<pid>/cmdline is whole command lines, and a Java or Node process clears 255 without trying. That is
an opt-in flag whose failure mode was silently losing everything.

These mirror `PushServerMetricsRequest`. They are duplicated across a network boundary, which is a real
cost — but the alternative is an agent that discovers the limits by wedging against them, and the server
must keep validating regardless because it does not only talk to our agent.
*/
const (
	maxCommand     = 255
	maxMountPoint  = 191
	maxDevice      = 191
	maxUnit        = 255
	maxDescription = 255
	maxState       = 32
	maxUsagePath   = 512
	maxUsageKind   = 64
	maxSystemField = 128
	maxFailReason  = 255
)

// clip shortens a string to at most n *bytes*, without splitting a UTF-8 character.
//
// Bytes rather than runes because that is what the server counts, and a mount point or a command line is
// arbitrary bytes chosen by somebody else — a path with an emoji in it is unusual and not invalid. Cutting
// mid-character would produce invalid UTF-8, which the JSON encoder would then mangle into a replacement
// character and the server would store.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
