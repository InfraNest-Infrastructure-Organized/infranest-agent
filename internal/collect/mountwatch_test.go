package collect

import (
	"sync/atomic"
	"testing"
	"time"
)

// The whole point of splitting the concurrency out is that this branch — the one a wedged NFS share takes
// — can be exercised under `-race`. Nothing else would ever reach it.
func TestProbeMountsGivesUpOnAMountThatNeverAnswers(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	entries := []mountEntry{
		{point: "/", device: "/dev/sda1"},
		{point: "/mnt/dead-nfs", device: "nfs:/export"},
		{point: "/var", device: "/dev/sda2"},
	}

	probe := func(e mountEntry) (Mount, bool) {
		if e.point == "/mnt/dead-nfs" {
			// A hard-mounted share whose server has gone: statfs does not error, it blocks forever.
			<-release
			return Mount{}, false
		}
		return Mount{MountPoint: e.point, Device: e.device, TotalBytes: 100, UsedBytes: 40}, true
	}

	start := time.Now()
	mounts, unanswered := probeMounts(entries, probe, 150*time.Millisecond)
	elapsed := time.Since(start)

	// The healthy mounts still get reported. Before this, one dead share hung the whole collection and
	// every metric on the machine went dark — which looks exactly like the server dying.
	if len(mounts) != 2 {
		t.Errorf("got %d mounts, want 2: %+v", len(mounts), mounts)
	}
	if len(unanswered) != 1 || unanswered[0] != "/mnt/dead-nfs" {
		t.Errorf("unanswered = %v, want [/mnt/dead-nfs]", unanswered)
	}
	if elapsed > time.Second {
		t.Errorf("took %v — the deadline did not hold", elapsed)
	}
}

// The abandoned goroutine unblocking later must not panic on a closed channel or block forever on a
// receiver that has gone. The channel is buffered to the number of probes for exactly this.
func TestAnAbandonedProbeCanStillFinishHarmlessly(t *testing.T) {
	release := make(chan struct{})
	var finished atomic.Bool

	entries := []mountEntry{{point: "/slow"}}
	probe := func(mountEntry) (Mount, bool) {
		<-release
		finished.Store(true)
		return Mount{MountPoint: "/slow", TotalBytes: 1, UsedBytes: 1}, true
	}

	if _, unanswered := probeMounts(entries, probe, 50*time.Millisecond); len(unanswered) != 1 {
		t.Fatalf("expected the slow mount to be unanswered")
	}

	close(release)
	for i := 0; i < 100 && !finished.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !finished.Load() {
		t.Error("the abandoned goroutine never completed — its send is blocking")
	}
}

func TestABlockedMountIsRetriedEventually(t *testing.T) {
	b := &mountBlocklist{until: map[string]time.Time{}}

	b.block("/mnt/dead")
	if !b.blocked("/mnt/dead") {
		t.Error("a mount that timed out should be skipped next cycle")
	}

	// A share whose server comes back should start being reported again without anyone restarting the
	// agent, so the block has to expire on its own.
	b.until["/mnt/dead"] = time.Now().Add(-time.Second)
	if b.blocked("/mnt/dead") {
		t.Error("the block did not expire")
	}
}
