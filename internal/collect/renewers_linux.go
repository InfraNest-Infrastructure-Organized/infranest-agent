//go:build linux

package collect

import (
	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/dbus"
)

// certbot's renewal timer under the names its packages give it: Debian/Ubuntu and EPEL, the snap, and
// Fedora's. The service beside each has the same base name.
var certbotUnits = []string{"certbot", "snap.certbot.renew", "certbot-renew"}

// CollectRenewers finds what renews certificates on this machine (#3149) and, for certbot, what systemd
// recorded about its last run. Never nil: an empty list is "nothing renews certificates here that we can
// see". The bus is asked only when certbot is installed, and a bus that cannot be reached costs the run
// dates, not the list.
func CollectRenewers() []Renewer {
	renewers := readRenewers("/")
	if !hasReadableTool(renewers, "certbot") {
		return renewers
	}

	conn, err := dbus.Dial(busTimeout)
	if err != nil {
		return renewers
	}
	defer func() { _ = conn.Close() }()

	units, err := conn.ListUnits()
	if err != nil {
		return renewers
	}
	paths := map[string]string{}
	for _, u := range units {
		paths[u.Name] = u.Path
	}

	for _, base := range certbotUnits {
		timer, ok := paths[base+".timer"]
		if !ok || timer == "" {
			continue
		}
		run := certbotRun{unit: base + ".timer"}
		if at, err := conn.TimerLastTrigger(timer); err == nil && !at.IsZero() {
			run.last = &at
		}
		if at, err := conn.TimerNextElapse(timer); err == nil && !at.IsZero() {
			run.next = &at
		}
		// The service keeps how its last run ended. A static oneshot that has run since boot stays loaded;
		// one that has not is absent from the list, and then the result is simply not known.
		if svc := paths[base+".service"]; svc != "" && run.last != nil {
			if result, err := conn.Result(svc); err == nil && result != "" {
				run.result = clip(result, maxState)
			}
		}
		applyCertbotRun(renewers, run)

		break
	}

	return renewers
}
