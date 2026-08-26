//go:build !linux

package collect

// isOtherFilesystem is Linux-only. Everywhere else the scan does not cross-check devices, which is
// acceptable because the disk-usage pass is a Linux feature — the Windows agent reports mounts as drive
// letters, where "underneath another mount" is not a thing that happens.
//
// Returning false rather than refusing to build: the walker itself is portable and its tests run on any
// machine, which is the same split the /proc parsers use.
func isOtherFilesystem(_, _ string) bool {
	return false
}
