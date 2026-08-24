package collect

import (
	"sync"
	"time"
)

// mountTimeout is how long every mount together gets to answer.
//
// statfs on a healthy filesystem returns in microseconds, so this is not a performance budget — it is the
// line past which a mount is presumed wedged. Two seconds is long enough that a busy disk under heavy load
// still answers, and short enough that a collection cycle never becomes noticeably late.
const mountTimeout = 2 * time.Second

// unresponsiveRetry is how long a wedged mount is skipped before it gets another chance. A share whose
// server comes back should start being reported again without anyone restarting the agent.
const unresponsiveRetry = 10 * time.Minute

// unresponsiveMounts remembers filesystems that failed to answer.
//
// A goroutine blocked in a syscall cannot be cancelled — Go has no way to interrupt one — so a mount that
// hangs leaks its goroutine for as long as the kernel keeps it in uninterruptible sleep. Without this,
// every collection cycle would start another one against the same dead share, and a machine with one
// unreachable NFS mount would accumulate goroutines until it ran out of memory. That would make the agent
// the cause of an outage, which is the one thing it must never be.
var unresponsiveMounts = &mountBlocklist{until: map[string]time.Time{}}

// probeMounts measures each mount concurrently and gives up on the ones that do not answer.
//
// The concurrency lives here, apart from the syscalls, so it can be tested with a probe that hangs on
// purpose — `go test -race` then actually exercises the timeout path, which is the branch a real wedged
// NFS share takes and the one nothing else would ever reach.
//
// Results come back over a buffered channel rather than a shared slice. The first version wrote into
// `results[i]` from each goroutine and read the slice after the deadline, which is a data race: an
// abandoned goroutine can still be writing its slot while the collector reads it. Buffering the channel
// to the number of probes means a goroutine that eventually unblocks can always complete its send
// instead of parking forever on a receiver that has gone.
func probeMounts(entries []mountEntry, probe func(mountEntry) (Mount, bool), timeout time.Duration) (mounts []Mount, unanswered []string) {
	type answer struct {
		point string
		mount Mount
		ok    bool
	}

	ch := make(chan answer, len(entries))
	started := map[string]bool{}

	for _, e := range entries {
		started[e.point] = true
		go func(e mountEntry) {
			m, ok := probe(e)
			ch <- answer{point: e.point, mount: m, ok: ok}
		}(e)
	}

	deadline := time.After(timeout)
	mounts = make([]Mount, 0, len(entries))

	for range entries {
		select {
		case a := <-ch:
			delete(started, a.point)
			if a.ok {
				mounts = append(mounts, a.mount)
			}
		case <-deadline:
			// Whatever has not answered is wedged in an uninterruptible syscall and is not coming back
			// on any timescale worth waiting for. Report what did answer — a full disk elsewhere still
			// needs to alert.
			for point := range started {
				unanswered = append(unanswered, point)
			}
			return mounts, unanswered
		}
	}

	return mounts, nil
}

type mountBlocklist struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (b *mountBlocklist) blocked(point string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	deadline, ok := b.until[point]
	if !ok {
		return false
	}
	if time.Now().After(deadline) {
		delete(b.until, point)
		return false
	}

	return true
}

func (b *mountBlocklist) block(point string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.until[point] = time.Now().Add(unresponsiveRetry)
}
