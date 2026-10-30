package collect

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// maxMountDisks bounds `mounts[].disks`. An md array or a volume group spans a handful of disks; eight is
// room for any of those, and mounts ride on every sample, so a cap is what keeps 64 mounts from carrying
// more names than readings.
const maxMountDisks = 8

// maxStackDepth bounds the walk down from a filesystem to its disks. A real stack is a few layers deep —
// LUKS on LVM on md on partitions is four — and the bound is what makes a cycle in a broken sysfs end.
const maxStackDepth = 8

// wholeDisks returns the whole disks the block device major:minor is stored on, read from sysfs under
// sys (`/sys` on a real machine; a directory of fixtures in a test).
func wholeDisks(sys string, major, minor uint64) []string {
	return wholeDisksFrom(sys, sys+"/dev/block/"+strconv.FormatUint(major, 10)+":"+strconv.FormatUint(minor, 10))
}

// wholeDisksNamed is the same walk started from a block device's kernel name (`vdb1`, `dm-0`), for a
// filesystem whose own device number is anonymous although it was mounted from a block device — btrfs
// gives every subvolume a device of its own with major 0. It finds the device the mount names, which for a
// btrfs spanning several devices is only the one it was mounted from.
func wholeDisksNamed(sys, name string) []string {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return nil
	}

	return wholeDisksFrom(sys, sys+"/class/block/"+name)
}

// wholeDisksFrom walks down from one block device's directory in sysfs.
//
// It follows the kernel's own links rather than parsing device names: /sys/dev/block/<major>:<minor> leads
// to the device, a partition's disk is the directory above it, and an LVM, dm-crypt or md device lists
// what it is built on in `slaves/`. A disk counts when /sys/block/<name>/device exists — the same rule the
// per-disk rows and the totals use, so a name here is always one that can appear in `disks[]`. A loop
// device or a zram disk has no device link and so yields nothing, which is right: its I/O is not a disk's.
//
// Every read is of world-readable sysfs. Nothing is opened under /dev, which the unit hides anyway.
func wholeDisksFrom(sys, from string) []string {
	start, err := filepath.EvalSymlinks(from)
	if err != nil {
		return nil
	}

	seen := map[string]bool{}
	var out []string

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxStackDepth || len(out) >= maxMountDisks {
			return
		}

		if fileExists(dir + "/partition") {
			walk(filepath.Dir(dir), depth+1)
			return
		}

		if slaves, err := os.ReadDir(dir + "/slaves"); err == nil && len(slaves) > 0 {
			for _, s := range slaves {
				if next, err := filepath.EvalSymlinks(dir + "/slaves/" + s.Name()); err == nil {
					walk(next, depth+1)
				}
			}
			return
		}

		name := filepath.Base(dir)
		if !seen[name] && fileExists(sys+"/block/"+name+"/device") {
			seen[name] = true
			out = append(out, clip(name, maxDiskName))
		}
	}
	walk(start, 0)

	sort.Strings(out)

	return out
}

// devNumbers splits a Linux dev_t into major and minor, the encoding glibc's gnu_dev_major/minor use.
func devNumbers(dev uint64) (major, minor uint64) {
	major = ((dev >> 8) & 0x00000fff) | ((dev >> 32) & 0xfffff000)
	minor = (dev & 0x000000ff) | ((dev >> 12) & 0xffffff00)

	return major, minor
}
