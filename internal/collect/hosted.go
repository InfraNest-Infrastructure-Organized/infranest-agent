package collect

import "sort"

// maxHostedServices bounds the service names one process carries. A shared svchost on a machine with little
// memory hosts a few dozen; past this the row is named by the first ones, alphabetically.
const maxHostedServices = 16

// maxHostedName clips one service's display name. Windows allows 256 characters, and a name long enough to
// need more than this says what it is well before the end.
const maxHostedName = 128

// attachHostedServices names, on each reported process, the running services it hosts (InfraNest #5670).
// `hosted` is process ID → display names, for every running service whatever its start type.
func attachHostedServices(procs []Process, hosted map[int][]string) {
	if len(hosted) == 0 {
		return
	}
	for i := range procs {
		names := hosted[procs[i].PID]
		if procs[i].PID <= 0 || len(names) == 0 {
			continue
		}
		clipped := make([]string, 0, len(names))
		for _, n := range names {
			if n = clip(n, maxHostedName); n != "" {
				clipped = append(clipped, n)
			}
		}
		sort.Strings(clipped)
		if len(clipped) > maxHostedServices {
			clipped = clipped[:maxHostedServices]
		}
		if len(clipped) > 0 {
			procs[i].Services = clipped
		}
	}
}
