//go:build !linux

package collect

import (
	"fmt"
	"runtime"
)

// CollectServices has nothing to report anywhere but Linux.
//
// An error rather than an empty list, for the reason every other collector on this platform returns one:
// an empty list is a claim — "nothing is failing" — and it is indistinguishable from a healthy machine.
// The server reads a missing `services` field as "no answer" and leaves the last known list standing.
func CollectServices() ([]Service, error) {
	return nil, fmt.Errorf("service units are a systemd concept and %s does not have one", runtime.GOOS)
}
