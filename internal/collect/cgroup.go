package collect

import (
	"encoding/hex"
	"strings"
)

// parseCgroupOwner names what a process belongs to from its /proc/<pid>/cgroup (InfraNest #4917): the
// systemd service it runs under, or the container it runs in. cgroup v2 has one "0::" line; v1 has one per
// controller, and the name=systemd or pids line carries the same path.
func parseCgroupOwner(body string) (unit string, containerID string) {
	for _, line := range strings.Split(body, "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := parts[2]
		segments := strings.Split(strings.Trim(path, "/"), "/")
		for i := len(segments) - 1; i >= 0; i-- {
			seg := segments[i]
			switch {
			case strings.HasPrefix(seg, "docker-") && strings.HasSuffix(seg, ".scope"):
				return "", shortID(strings.TrimSuffix(strings.TrimPrefix(seg, "docker-"), ".scope"))
			case strings.HasPrefix(seg, "libpod-") && strings.HasSuffix(seg, ".scope"):
				return "", shortID(strings.TrimSuffix(strings.TrimPrefix(seg, "libpod-"), ".scope"))
			case i > 0 && segments[i-1] == "docker" && isHex(seg):
				return "", shortID(seg) // the cgroupfs driver: /docker/<id>
			case strings.HasSuffix(seg, ".service"):
				return clip(seg, maxUnit), ""
			}
		}
	}
	return "", ""
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func isHex(s string) bool {
	if len(s) < 12 {
		return false
	}
	_, err := hex.DecodeString(s[:len(s)-len(s)%2])
	return err == nil
}
