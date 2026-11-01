//go:build !windows

package jobs

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// readReport opens a report without following a link, checks it is one plain file with one name, and says who owns
// it. A hard link to a file of the agent's own — its token — would otherwise be read as a report; the kernel refuses
// to make one to a file you do not own (fs.protected_hardlinks), and the link count refuses the rest.
func readReport(path string) (Report, string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Report{}, "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Report{}, "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || st.Nlink != 1 || info.Size() > MaxReportBytes {
		return Report{}, "", errors.New("not a report")
	}
	body, err := io.ReadAll(io.LimitReader(f, MaxReportBytes+1))
	if err != nil || len(body) > MaxReportBytes {
		return Report{}, "", errors.New("not a report")
	}
	var r Report
	if err := json.Unmarshal(body, &r); err != nil {
		return Report{}, "", err
	}

	return r, ownerName(st.Uid), nil
}

// ownerName is the user a uid belongs to, from the account database — the number when it is not listed.
func ownerName(uid uint32) string {
	id := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(id); err == nil && u.Username != "" {
		return u.Username
	}

	return id
}
