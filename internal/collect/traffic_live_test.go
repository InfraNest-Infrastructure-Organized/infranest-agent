//go:build linux

package collect

import (
	"os"
	"testing"
	"time"
)

// Reads the real counters twice around some disk writes. Opt-in, because it needs a real Linux machine
// with a real disk: INFRANEST_LIVE_TRAFFIC=1 go test -run Live ./internal/collect/
func TestLiveTraffic(t *testing.T) {
	if os.Getenv("INFRANEST_LIVE_TRAFFIC") == "" {
		t.Skip("set INFRANEST_LIVE_TRAFFIC=1 to read this machine's counters")
	}

	tr := NewTrafficTracker()
	tr.observe(readTraffic())

	f, err := os.CreateTemp(t.TempDir(), "w")
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for i := 0; i < 64; i++ {
		_, _ = f.Write(buf)
	}
	_ = f.Sync()
	_ = f.Close()
	time.Sleep(1500 * time.Millisecond)

	in, out, read, write := tr.observe(readTraffic())
	t.Logf("in=%v out=%v read=%v write=%v", deref(in), deref(out), deref(read), deref(write))
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}

	return *p
}
