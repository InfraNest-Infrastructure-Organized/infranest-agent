//go:build windows

package jobs

import "errors"

// Reports are not collected on Windows yet: the folder needs an access list that lets anyone add a file and nobody
// read one, and the agent ships with no Windows folder for it. Dir answers "" there, so this is never reached.
func readReport(string) (Report, string, error) {
	return Report{}, "", errors.New("job reports are not collected on Windows yet")
}
