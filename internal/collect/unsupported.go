//go:build !linux && !windows

package collect

import (
	"fmt"
	"net/netip"
	"runtime"
	"time"
)

// Options mirrors the Linux build so the rest of the agent compiles and tests run everywhere. The fields
// mean the same thing; there is simply nothing here that reads them yet.
type Options struct {
	Processes    bool
	ProcessArgs  bool
	MaxProcesses int
	CPUInterval  time.Duration
	Services     bool
	Containers   *ContainerTracker
	Renewers     bool
	Traffic      *TrafficTracker
	Listening    bool
	KnownPeers   map[netip.Addr]struct{}
	Packages     *PackageIndex
	// Cron reads the cron definitions any user can read (InfraNest #5801), cached until a file changes. Skipped when
	// nil, and read on Linux only.
	Cron *CronReader
	// Setup is how the machine is set up (InfraNest #5815): never read on this platform. Accepted so the run loop builds
	// one Options for every platform.
	Setup *SetupReader
}

// Collect reports honestly that this platform is not implemented rather than returning zeros.
//
// A sample full of zeros would be indistinguishable from an idle machine, and the server would average it
// into a rule's window. Saying nothing is the truthful answer.
func Collect(Options) (Sample, error) {
	return Sample{CollectedAt: time.Now().UTC()}, fmt.Errorf("collecting is not implemented on %s yet", runtime.GOOS)
}
