//go:build linux

package collect

import (
	"os"
	"syscall"
)

// isOtherFilesystem reports whether `path` sits on a different device from `root`.
//
// Without this, a scan of `/` walks straight into every mount beneath it and attributes their contents to
// the root filesystem — so a 2 TB data volume mounted at /var/lib/docker would be reported as filling a
// 40 GB root disk. The answer would be both alarming and impossible to act on, because the directory it
// names is not on the disk it claims to be filling.
//
// Device numbers rather than a mount table read: `/proc/mounts` would have to be parsed, kept in sync
// during a walk that can take seconds, and would still miss a bind mount of the same filesystem. The
// kernel already knows.
func isOtherFilesystem(root, path string) bool {
	rootStat, err := os.Lstat(root)
	if err != nil {
		return false
	}

	pathStat, err := os.Lstat(path)
	if err != nil {
		return false
	}

	rootSys, ok1 := rootStat.Sys().(*syscall.Stat_t)
	pathSys, ok2 := pathStat.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false
	}

	return rootSys.Dev != pathSys.Dev
}
