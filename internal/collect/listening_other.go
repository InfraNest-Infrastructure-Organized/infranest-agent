//go:build !linux

package collect

// ListeningAvailable is false off Linux: the listening collector reads /proc/net, and on Windows the
// `listening` field is absent rather than approximated (InfraNest #4019).
func ListeningAvailable() bool { return false }
