//go:build linux

package collect

import (
	"io"
	"os"
)

// readPressure fills the pressure fields from /proc/pressure, on kernels built with PSI (4.20+, and on by
// default in the major distributions). Absent everywhere else, which is not "no pressure".
func readPressure(s *Sample) {
	read := func(resource string) (*float64, *float64) {
		f, err := os.Open("/proc/pressure/" + resource)
		if err != nil {
			return nil, nil
		}
		defer func() { _ = f.Close() }()

		return parsePressure(io.LimitReader(f, 4096))
	}

	// CPU's `full` line is always zero system-wide (a CPU cannot be fully stalled on itself), so only
	// `some` is worth sending.
	s.PressureCPU, _ = read("cpu")
	s.PressureMemory, s.PressureMemoryFull = read("memory")
	s.PressureIO, s.PressureIOFull = read("io")
}
