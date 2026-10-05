package collect

import (
	"strings"
	"testing"
	"time"
)

/*
Container restarts, counted from systemd's scope units (#2809). The agent never asks the Docker socket, so
the only evidence of a restart is a scope that came back with a later start time — which is what these pin.
*/

const scopeID = "3f4e5d6c7b8a99887766554433221100ffeeddccbbaa00998877665544332211"

func TestADockerScopeIsAContainer(t *testing.T) {
	id, rt, ok := containerScope("docker-" + scopeID + ".scope")

	if !ok || id != "3f4e5d6c7b8a" || rt != "docker" {
		t.Fatalf("id=%q runtime=%q ok=%v", id, rt, ok)
	}
}

func TestAPodmanScopeIsAContainer(t *testing.T) {
	if _, rt, ok := containerScope("libpod-" + scopeID + ".scope"); !ok || rt != "podman" {
		t.Fatalf("runtime=%q ok=%v", rt, ok)
	}
}

func TestOtherScopesAreNotContainers(t *testing.T) {
	for _, unit := range []string{
		"session-3.scope",
		"docker-" + scopeID + ".service",
		"docker-" + strings.ToUpper(scopeID) + ".scope",
		"docker-" + scopeID[:63] + ".scope",
		"docker-" + scopeID[:63] + "g.scope",
		"docker.service",
	} {
		if _, _, ok := containerScope(unit); ok {
			t.Fatalf("%q read as a container", unit)
		}
	}
}

func TestTheFirstSightingIsABaseline(t *testing.T) {
	tr := NewContainerTracker()
	now := time.Now()

	// It may have restarted forty times before the agent started; when is unknowable.
	if n := tr.observe("a", now.Add(-time.Hour), now); n != 0 {
		t.Fatalf("restarts=%d", n)
	}
}

func TestALaterStartIsOneRestart(t *testing.T) {
	tr := NewContainerTracker()
	t0 := time.Now()

	tr.observe("a", t0, t0)
	tr.observe("a", t0, t0.Add(time.Minute)) // same incarnation
	n := tr.observe("a", t0.Add(90*time.Second), t0.Add(2*time.Minute))

	if n != 1 {
		t.Fatalf("restarts=%d", n)
	}
	if n := tr.observe("a", t0.Add(150*time.Second), t0.Add(3*time.Minute)); n != 2 {
		t.Fatalf("restarts=%d", n)
	}
}

func TestAStartTimeThatGoesBackIsNotARestart(t *testing.T) {
	tr := NewContainerTracker()
	t0 := time.Now()

	tr.observe("a", t0, t0)
	if n := tr.observe("a", t0.Add(-time.Second), t0.Add(time.Minute)); n != 0 {
		t.Fatalf("a clock step counted as a restart: %d", n)
	}
}

func TestAContainerAbsentBetweenRestartsIsStillTracked(t *testing.T) {
	tr := NewContainerTracker()
	t0 := time.Now()

	tr.observe("a", t0, t0)
	// Docker's backoff leaves it down for a while; it is missing from these collections.
	tr.forget(t0.Add(5 * time.Minute))
	if n := tr.observe("a", t0.Add(5*time.Minute), t0.Add(5*time.Minute)); n != 1 {
		t.Fatalf("restarts=%d", n)
	}
}

func TestAContainerGoneLongEnoughIsForgotten(t *testing.T) {
	tr := NewContainerTracker()
	t0 := time.Now()

	tr.observe("a", t0, t0)
	tr.forget(t0.Add(trackerForget + time.Minute))

	if _, ok := tr.seen["a"]; ok {
		t.Fatal("the tracker grows without bound")
	}
}
