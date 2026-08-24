//go:build windows

package collect

import (
	"fmt"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

// Windows collection, through the stdlib `syscall` package only.
//
// The obvious library for this is golang.org/x/sys/windows, and it would be less code. It is not used
// because a dependency-free agent is a claim this project makes and checks in CI, and the alternative is
// a few dozen lines of very mechanical DLL binding. `syscall.NewLazyDLL` is in the standard library and
// resolves on first use, so nothing here costs anything on a machine that never calls it.
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	psapi    = syscall.NewLazyDLL("psapi.dll")

	procGetSystemTimes         = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx   = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64         = kernel32.NewProc("GetTickCount64")
	procGetLogicalDriveStrings = kernel32.NewProc("GetLogicalDriveStringsW")
	procGetDriveType           = kernel32.NewProc("GetDriveTypeW")
	procGetDiskFreeSpaceEx     = kernel32.NewProc("GetDiskFreeSpaceExW")
	procQueryFullProcessImage  = kernel32.NewProc("QueryFullProcessImageNameW")

	procEnumProcesses        = psapi.NewProc("EnumProcesses")
	procGetProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

const (
	driveFixed = 3 // DRIVE_FIXED

	processQueryLimitedInformation = 0x1000
)

// Options mirrors the Linux build. Load average has no field here because Windows has no load average —
// see Collect.
//
// ProcessArgs is accepted and currently has no effect on Windows: reading another process's command line
// means ReadProcessMemory against its PEB, which needs more privilege than this agent asks for and is
// exactly the kind of capability it should not acquire. The flag is documented as Linux-only rather than
// silently doing nothing, which is what it did before.
type Options struct {
	Processes    bool
	ProcessArgs  bool
	MaxProcesses int
	CPUInterval  time.Duration
}

// Collect takes one reading on Windows.
//
// Load average is absent, and deliberately not synthesised. It is not "hard to obtain" on Windows — the
// concept does not exist. Processor Queue Length is sometimes offered as an equivalent and is not one: it
// counts threads waiting, not a decaying average of runnable work, so a threshold set against a Linux
// intuition would mean something entirely different. A null says "not measured", which the server already
// excludes from a rule's window rather than averaging as zero. A fabricated number would quietly fire
// alerts nobody could interpret.
//
// Swap is absent for the same reason. Windows has a page file, not swap, and the arithmetic usually
// offered for "swap used" is commit charge minus physical, which is an approximation of a different thing.
func Collect(opts Options) (Sample, error) {
	s := Sample{CollectedAt: time.Now().UTC()}

	if pct, err := collectCPUWindows(opts.CPUInterval); err != nil {
		s.fail("cpu", err)
	} else {
		s.CPUPercent = f64(pct)
	}

	if used, total, err := memoryWindows(); err != nil {
		s.fail("memory", err)
	} else {
		s.MemoryUsed, s.MemoryTotal = i64(used), i64(total)
		s.MemoryPercent = f64(clampPercent(float64(used) / float64(total) * 100))
	}

	if up, err := uptimeWindows(); err != nil {
		s.fail("uptime", err)
	} else {
		s.Uptime = i64(up)
	}

	if mounts, err := drivesWindows(); err != nil {
		s.fail("mounts", err)
	} else {
		s.Mounts = mounts
	}

	if opts.Processes {
		if procs, err := processesWindows(opts); err != nil {
			s.fail("processes", err)
		} else {
			s.Processes = procs
		}
	}

	return s, nil
}

// filetime is a 64-bit count of 100-nanosecond intervals, split across two 32-bit halves.
type filetime struct{ low, high uint32 }

func (f filetime) uint64() uint64 { return uint64(f.high)<<32 | uint64(f.low) }

// windowsCPUTimes is the shape GetSystemTimes reports. Note that kernel time *includes* idle — a detail
// that silently halves or doubles the answer if it is missed.
type windowsCPUTimes struct{ idle, kernel, user uint64 }

func (c windowsCPUTimes) total() uint64 { return c.kernel + c.user }
func (c windowsCPUTimes) busy() uint64 {
	if c.total() < c.idle {
		return 0
	}
	return c.total() - c.idle
}

func systemTimes() (windowsCPUTimes, error) {
	var idle, kernel, user filetime
	r, _, err := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r == 0 {
		return windowsCPUTimes{}, fmt.Errorf("GetSystemTimes: %w", err)
	}

	return windowsCPUTimes{idle: idle.uint64(), kernel: kernel.uint64(), user: user.uint64()}, nil
}

func collectCPUWindows(interval time.Duration) (float64, error) {
	if interval <= 0 {
		interval = 300 * time.Millisecond
	}

	before, err := systemTimes()
	if err != nil {
		return 0, err
	}

	time.Sleep(interval)

	after, err := systemTimes()
	if err != nil {
		return 0, err
	}

	// Same guard as Linux, for the same reason: both subtractions are unsigned, and a counter that goes
	// backwards must refuse rather than wrap into a confident 100%.
	if after.total() < before.total() || after.busy() < before.busy() {
		return 0, fmt.Errorf("cpu counters went backwards")
	}

	total := after.total() - before.total()
	if total == 0 {
		return 0, fmt.Errorf("no time elapsed between cpu samples")
	}

	return clampPercent(float64(after.busy()-before.busy()) / float64(total) * 100), nil
}

// memoryStatusEx mirrors MEMORYSTATUSEX. The length field must be set before the call or it fails.
type memoryStatusEx struct {
	length               uint32
	memoryLoad           uint32
	totalPhys            uint64
	availPhys            uint64
	totalPageFile        uint64
	availPageFile        uint64
	totalVirtual         uint64
	availVirtual         uint64
	availExtendedVirtual uint64
}

func memoryWindows() (used, total int64, err error) {
	var st memoryStatusEx
	st.length = uint32(unsafe.Sizeof(st))

	r, _, callErr := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if r == 0 {
		return 0, 0, fmt.Errorf("GlobalMemoryStatusEx: %w", callErr)
	}
	if st.totalPhys == 0 {
		return 0, 0, fmt.Errorf("GlobalMemoryStatusEx reported no physical memory")
	}

	return int64(st.totalPhys - st.availPhys), int64(st.totalPhys), nil
}

func uptimeWindows() (int64, error) {
	// GetTickCount64 cannot fail and returns milliseconds since boot.
	ms, _, _ := procGetTickCount64.Call()

	return int64(ms / 1000), nil
}

// drivesWindows reports the fixed drives. Removable, network and optical drives are skipped: a CD-ROM at
// 100% is a disc, and a disconnected network drive is somebody else's disk.
func drivesWindows() ([]Mount, error) {
	buf := make([]uint16, 512)
	n, _, err := procGetLogicalDriveStrings.Call(uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if n == 0 {
		return nil, fmt.Errorf("GetLogicalDriveStringsW: %w", err)
	}

	out := make([]Mount, 0, 8)
	for _, root := range splitNulUTF16(buf[:n]) {
		rootPtr, convErr := syscall.UTF16PtrFromString(root)
		if convErr != nil {
			continue
		}

		if t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(rootPtr))); t != driveFixed {
			continue
		}

		var freeForCaller, totalBytes, totalFree uint64
		r, _, _ := procGetDiskFreeSpaceEx.Call(
			uintptr(unsafe.Pointer(rootPtr)),
			uintptr(unsafe.Pointer(&freeForCaller)),
			uintptr(unsafe.Pointer(&totalBytes)),
			uintptr(unsafe.Pointer(&totalFree)),
		)
		if r == 0 || totalBytes == 0 {
			continue
		}

		out = append(out, Mount{
			MountPoint: root,
			Device:     root,
			TotalBytes: int64(totalBytes),
			// freeForCaller, not totalFree: on a volume with quotas those differ, and the number that
			// matters is what this account can still write.
			UsedBytes: int64(totalBytes - freeForCaller),
		})
	}

	return out, nil
}

// processMemoryCounters mirrors PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func processesWindows(opts Options) ([]Process, error) {
	pids := make([]uint32, 4096)
	var needed uint32

	r, _, err := procEnumProcesses.Call(
		uintptr(unsafe.Pointer(&pids[0])),
		uintptr(len(pids)*4),
		uintptr(unsafe.Pointer(&needed)),
	)
	if r == 0 {
		return nil, fmt.Errorf("EnumProcesses: %w", err)
	}

	procs := make([]Process, 0, 64)
	for _, pid := range pids[:needed/4] {
		if pid == 0 {
			continue
		}

		// PROCESS_QUERY_LIMITED_INFORMATION is the least this needs. Running unprivileged, most processes
		// belonging to other accounts will refuse to open at all — which is correct, and simply means they
		// are skipped rather than the collector failing.
		h, openErr := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
		if openErr != nil {
			continue
		}

		p := Process{PID: int(pid)}

		if name, nameErr := processImageName(h); nameErr == nil {
			p.Command = name
		}

		var counters processMemoryCounters
		counters.cb = uint32(unsafe.Sizeof(counters))
		if ok, _, _ := procGetProcessMemoryInfo.Call(
			uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb),
		); ok != 0 {
			p.MemoryBytes = i64(int64(counters.workingSetSize))
		}

		syscall.CloseHandle(h)

		if p.Command == "" {
			// The name could not be read — usually another account's process. Reporting it by pid is more
			// honest than dropping it: something is using that memory, and a list that silently omits the
			// largest consumer answers "what is eating this box" wrongly.
			p.Command = fmt.Sprintf("pid %d", pid)
		}
		procs = append(procs, p)
	}

	return topByMemory(procs, opts.MaxProcesses), nil
}

// processImageName returns the executable name, without its path or arguments.
//
// The full path is deliberately trimmed to the base name, matching Linux, where the default is the `comm`
// field rather than the command line. A path can itself be revealing — a directory named after a customer,
// or a build under someone's home directory — and the agent's default is to say as little as answers the
// question.
func processImageName(h syscall.Handle) (string, error) {
	// 32767, not MAX_PATH. Windows has allowed longer paths since 1607, and a process installed under a
	// deep path would otherwise fail here — after which the caller drops it entirely, so the busiest
	// process on the machine could be the one silently missing from the list.
	buf := make([]uint16, 32768)
	size := uint32(len(buf))

	r, _, err := procQueryFullProcessImage.Call(
		uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return "", fmt.Errorf("QueryFullProcessImageNameW: %w", err)
	}

	return baseName(string(utf16.Decode(buf[:size]))), nil
}
