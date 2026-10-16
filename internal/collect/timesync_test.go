package collect

import "testing"

// Clock sync (InfraNest #3832): the kernel's own view, which chrony, ntpd and timesyncd all feed.
func TestASynchronisedClockReportsItsErrorBound(t *testing.T) {
	synced, errMs := timeSyncFrom(0, 0x2001, 12_500)
	if synced == nil || !*synced || *errMs != 12 {
		t.Fatalf("got %v %v", derefBool(synced), *errMs)
	}
}

func TestAnUnsynchronisedClockIsSaid(t *testing.T) {
	if synced, _ := timeSyncFrom(5, 0, 0); *synced {
		t.Fatal("TIME_ERROR is not synchronised")
	}
	if synced, _ := timeSyncFrom(0, staUnsync, 16_000_000); *synced {
		t.Fatal("STA_UNSYNC is not synchronised")
	}
}

func derefBool(b *bool) any {
	if b == nil {
		return nil
	}

	return *b
}
