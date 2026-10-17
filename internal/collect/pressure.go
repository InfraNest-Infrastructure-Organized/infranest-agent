package collect

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// parsePressure reads one /proc/pressure file (InfraNest #3827) and returns its `some` and `full` avg60:
// the share of the last minute in which some, or all, runnable tasks were stalled on this resource.
//
//	some avg10=0.00 avg60=1.23 avg300=0.50 total=123456
//	full avg10=0.00 avg60=0.40 avg300=0.10 total=45678
//
// Load average counts tasks; this says how long work actually waited, and on what — which is the question
// a "struggling" alert is really asking. avg60 because the server's window averages minute samples anyway.
func parsePressure(r io.Reader) (some, full *float64) {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		kind, rest, ok := strings.Cut(scanner.Text(), " ")
		if !ok || (kind != "some" && kind != "full") {
			continue
		}
		for _, field := range strings.Fields(rest) {
			name, value, ok := strings.Cut(field, "=")
			if !ok || name != "avg60" {
				continue
			}
			v, err := strconv.ParseFloat(value, 64)
			if err != nil || v < 0 || v > 100 {
				continue
			}
			if kind == "some" {
				some = &v
			} else {
				full = &v
			}
		}
	}

	return some, full
}
