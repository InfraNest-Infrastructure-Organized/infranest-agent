//go:build linux

package collect

import (
	"encoding/binary"
	"io"
	"net/netip"
	"os"
)

// ListeningAvailable reports whether this platform has a listening collector, so `collectors.listening`
// says "off" rather than "on and empty" where there is none.
func ListeningAvailable() bool { return true }

// CollectListening reads /proc/net/{tcp,tcp6,udp,udp6} (InfraNest #4019).
//
// World-readable, and per network namespace: these are the sockets of the namespace the agent runs in,
// which under the shipped unit is the host's — it sets no PrivateNetwork. A container with its own
// namespace has its own tables, which are not read here; what such a container publishes appears only if
// something on the host listens for it (docker-proxy does; Docker with the userland proxy off does not).
//
// No netlink (sock_diag), which `ss` uses: the unit's RestrictAddressFamilies forbids AF_NETLINK, and a
// forbidden family is a refused socket(), not a crash — but /proc answers the same question with no new
// permission at all.
//
// known is the set of this organization's other servers' addresses from the receiver (InfraNest #4903), or
// nil when the agent holds none; only connections to those addresses are reported by address.
func CollectListening(known map[netip.Addr]struct{}) (*Listening, error) {
	return collectListening(func(name string) (io.ReadCloser, error) {
		return os.Open("/proc/net/" + name)
	}, binary.NativeEndian, known)
}
