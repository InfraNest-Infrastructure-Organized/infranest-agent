//go:build linux

package collect

import (
	"testing"

	"github.com/InfraNest-Infrastructure-Organized/infranest-agent/internal/dbus"
)

// A failed per-connection instance of socket-activated sshd is named after the client (InfraNest #5864): it is not
// watched, while the service it belongs to and other templates' instances still are.
func TestALoginServicesConnectionInstanceIsNotWatched(t *testing.T) {
	cases := map[string]bool{
		"sshd@3-10.0.0.1:22-203.0.113.7:51234.service":      false,
		"ssh@0-[2001:db8::1]:22-[2001:db8::7]:4242.service": false,
		"sshd@.service":              false, // a template
		"sshd.service":               true,
		"ssh.service":                true,
		"postgresql@16-main.service": true,
		"nginx.service":              true,
		"sshd.socket":                false, // sockets are plumbing, as before
	}
	for name, want := range cases {
		if got := watchable(dbus.Unit{Name: name, LoadState: "loaded", ActiveState: "failed"}); got != want {
			t.Errorf("watchable(%s) = %v, want %v", name, got, want)
		}
	}
}
