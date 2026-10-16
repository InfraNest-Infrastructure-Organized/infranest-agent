package collect

import (
	"bufio"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// RaidArray is one Linux software RAID array, from /proc/mdstat (InfraNest #3833).
//
// A mirror that loses a disk keeps serving from the other one, and nothing on the machine says so loudly:
// the realistic story on a dedicated server with md RAID1 is a disk that dies unnoticed and a second that
// takes the server with it months later. /proc/mdstat is world-readable, so this needs no privilege.
type RaidArray struct {
	Name  string `json:"name"`            // md0
	Level string `json:"level,omitempty"` // raid1, raid5, …
	// `active` or `inactive` — an inactive array is assembled from too few members to run at all.
	State string `json:"state"`
	// Members the array is built for, and how many are in it and working: [2/1] is 2 and 1.
	Devices int `json:"devices,omitempty"`
	Active  int `json:"active,omitempty"`
	// Members marked (F): failed and still listed.
	Failed int `json:"failed,omitempty"`
	// A resync, recovery (rebuilding onto a replacement) or check in progress, and how far it is.
	Sync        string   `json:"sync,omitempty"`
	SyncPercent *float64 `json:"sync_percent,omitempty"`
}

// Degraded is whether the array is running on fewer members than it was built for.
func (a RaidArray) Degraded() bool { return a.Devices > 0 && a.Active < a.Devices }

var (
	mdHeader = regexp.MustCompile(`^(md\S*)\s*:\s*(active|inactive)(?:\s+\(\S+\))?\s*(raid\d+|linear|multipath)?\s*(.*)$`)
	mdCounts = regexp.MustCompile(`\[(\d+)/(\d+)\]`)
	mdSync   = regexp.MustCompile(`(resync|recovery|reshape|check|repair)\s*=\s*([\d.]+)%`)
)

// parseMdstat reads /proc/mdstat. Arrays with no redundancy (raid0, linear) are still listed, with no
// member counts — there is nothing to degrade.
func parseMdstat(r io.Reader) []RaidArray {
	var out []RaidArray
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := scanner.Text()

		if m := mdHeader.FindStringSubmatch(line); m != nil {
			a := RaidArray{Name: clip(m[1], maxState), State: m[2], Level: m[3]}
			a.Failed = strings.Count(m[4], "(F)")
			out = append(out, a)

			continue
		}

		if len(out) == 0 {
			continue
		}
		cur := &out[len(out)-1]

		if m := mdCounts.FindStringSubmatch(line); m != nil && cur.Devices == 0 {
			cur.Devices, _ = strconv.Atoi(m[1])
			cur.Active, _ = strconv.Atoi(m[2])
		}
		if m := mdSync.FindStringSubmatch(line); m != nil {
			cur.Sync = m[1]
			if p, err := strconv.ParseFloat(m[2], 64); err == nil {
				cur.SyncPercent = &p
			}
		}
	}

	if len(out) > 32 {
		out = out[:32]
	}

	return out
}
