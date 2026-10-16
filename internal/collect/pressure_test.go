package collect

import (
	"strings"
	"testing"
)

// Pressure stall information (InfraNest #3827), in the format Documentation/accounting/psi.rst gives.
func TestPressureIsTheMinuteAverage(t *testing.T) {
	some, full := parsePressure(strings.NewReader(
		"some avg10=4.67 avg60=12.25 avg300=3.10 total=29103474\nfull avg10=1.00 avg60=6.50 avg300=0.90 total=14101523\n"))
	if some == nil || *some != 12.25 || full == nil || *full != 6.5 {
		t.Fatalf("got %v %v", some, full)
	}
}

func TestAKernelWithoutAFullLineReportsOnlySome(t *testing.T) {
	// CPU before 5.13 has no `full` line at all.
	some, full := parsePressure(strings.NewReader("some avg10=0.00 avg60=0.50 avg300=0.10 total=1000\n"))
	if some == nil || full != nil {
		t.Fatalf("got %v %v", some, full)
	}
}

func TestGarbageIsNotAMeasurement(t *testing.T) {
	if some, full := parsePressure(strings.NewReader("some avg60=abc\nfull avg60=250\n")); some != nil || full != nil {
		t.Fatal("unparseable or impossible figures were sent")
	}
}
