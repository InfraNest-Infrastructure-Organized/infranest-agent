//go:build !linux && !windows

package collect

import (
	"fmt"
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
}

// Collect reports honestly that this platform is not implemented rather than returning zeros.
//
// A sample full of zeros would be indistinguishable from an idle machine, and the server would average it
// into a rule's window. Saying nothing is the truthful answer.
func Collect(Options) (Sample, error) {
	return Sample{CollectedAt: time.Now().UTC()}, fmt.Errorf("collecting is not implemented on %s yet", runtime.GOOS)
}
