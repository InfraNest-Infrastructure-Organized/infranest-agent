package collect

import (
	"sync"
	"time"
)

// Container is one running container, as systemd sees it (#2809).
//
// Under Docker's systemd cgroup driver — the default on cgroup v2 — and under Podman, every running
// container is a transient `docker-<id>.scope` or `libpod-<id>.scope` unit. That is the whole source: the
// agent never talks to the Docker socket, which is root on the host and would end the promise that it runs
// unprivileged and can command nothing. The price is that a container is known by its ID, not its name —
// the name lives in the runtime's own state, which only root can read. `docker ps --filter id=<id>` on the
// host turns one into the other.
type Container struct {
	// The first twelve hex digits of the container ID, as `docker ps` prints them.
	ID string `json:"id"`

	// "docker" or "podman", from the scope's prefix.
	Runtime string `json:"runtime"`

	// systemd's word for the scope's state — almost always `active`, since a stopped container's scope is
	// gone rather than inactive.
	ActiveState string `json:"active_state"`

	// When this incarnation of the container started: the scope's `ActiveEnterTimestamp`. A restart is a
	// new scope, so this moves on every restart and is what the restart count below is counted from.
	StartedAt *time.Time `json:"started_at,omitempty"`

	// How many times this agent has seen the container start again since it began watching it.
	//
	// A counter, like a service's `NRestarts`, and a lower bound: it is counted once per collection, so
	// three restarts between two readings count as one. That is enough for the question it answers — "is
	// this container in a restart loop" — where the loop is the signal and its exact speed is not. It starts
	// at zero whenever the agent starts, which the receiver reads as a counter reset, not as a fall.
	Restarts *uint64 `json:"restarts,omitempty"`

	// What the container's cgroup is using, in bytes. Absent where systemd keeps no memory accounting.
	MemoryBytes *uint64 `json:"memory_bytes,omitempty"`
}

// maxContainers bounds one collection, matching the server's cap. A host running more than this is a
// cluster node, and losing the tail is better than a payload the server refuses whole.
const maxContainers = 100

// trackerForget is how long a container may be missing before the tracker forgets it. A crash-looping
// container is absent between restarts — Docker backs off for up to a minute — so this has to outlast that
// comfortably; a container gone for longer than this and back again is a fresh start, not a restart loop.
const trackerForget = 30 * time.Minute

// ContainerTracker remembers each container's last start time between collections, which is the only way
// to count restarts without asking the runtime: a restart is a scope that came back with a later start.
//
// Owned by the run loop and passed in through Options. Bounded by `trackerForget`, so a host that churns
// through short-lived containers does not grow it without limit.
type ContainerTracker struct {
	mu   sync.Mutex
	seen map[string]*tracked
}

type tracked struct {
	startedAt time.Time
	lastSeen  time.Time
	restarts  uint64
}

// NewContainerTracker returns an empty tracker.
func NewContainerTracker() *ContainerTracker {
	return &ContainerTracker{seen: map[string]*tracked{}}
}

// observe records one sighting and returns the restart count so far.
//
// The first sighting is a baseline: whatever restarted before we were watching has no time we could put
// on it. A start time that moved forward is one restart. One that did not move, or moved backwards (a
// clock step), is not.
func (t *ContainerTracker) observe(id string, startedAt, now time.Time) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	entry, ok := t.seen[id]
	if !ok {
		t.seen[id] = &tracked{startedAt: startedAt, lastSeen: now}

		return 0
	}

	if startedAt.After(entry.startedAt) {
		entry.restarts++
		entry.startedAt = startedAt
	}
	entry.lastSeen = now

	return entry.restarts
}

// forget drops containers not seen for `trackerForget`.
func (t *ContainerTracker) forget(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for id, entry := range t.seen {
		if now.Sub(entry.lastSeen) > trackerForget {
			delete(t.seen, id)
		}
	}
}

// containerScope recognises a container's scope unit and returns its short ID and runtime.
//
// Strict on purpose: a scope name is chosen by whoever created it, and anything that is not exactly a
// runtime prefix and a 64-digit hex ID is somebody else's scope, not a container.
func containerScope(unit string) (id, runtime string, ok bool) {
	for prefix, rt := range map[string]string{"docker-": "docker", "libpod-": "podman"} {
		if len(unit) != len(prefix)+64+len(".scope") || unit[:len(prefix)] != prefix || unit[len(unit)-6:] != ".scope" {
			continue
		}

		hex := unit[len(prefix) : len(prefix)+64]
		for _, c := range hex {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return "", "", false
			}
		}

		return hex[:12], rt, true
	}

	return "", "", false
}
