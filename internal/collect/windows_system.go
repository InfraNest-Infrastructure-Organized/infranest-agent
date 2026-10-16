//go:build windows

package collect

import (
	"runtime"
	"syscall"
	"unsafe"
)

// The System card and the service rules on Windows (InfraNest #3837): what the machine says about itself,
// whether it is waiting for a reboot, and which of its automatic services have stopped with an error.
// Everything here is readable without administrator rights — the registry keys below are readable by any
// user, and the Service Control Manager lets anyone enumerate services and read their configuration.

var (
	advapi32                 = syscall.NewLazyDLL("advapi32.dll")
	procOpenSCManager        = advapi32.NewProc("OpenSCManagerW")
	procEnumServicesStatusEx = advapi32.NewProc("EnumServicesStatusExW")
	procOpenService          = advapi32.NewProc("OpenServiceW")
	procQueryServiceConfig   = advapi32.NewProc("QueryServiceConfigW")
	procCloseServiceHandle   = advapi32.NewProc("CloseServiceHandle")
)

const (
	keyWow6464Key            = 0x0100
	scManagerEnumerate       = 0x0004
	serviceQueryConfig       = 0x0001
	scEnumProcessInfo        = 0
	serviceWin32             = 0x30
	serviceStateAll          = 3
	errorMoreData            = 234
	errorInsufficientBuffer  = 122
	serviceAutoStart         = 2
	serviceStopped           = 1
	serviceStartPending      = 2
	serviceStopPending       = 3
	serviceRunning           = 4
	errorServiceNeverStarted = 1077
	maxWindowsServices       = 200
)

// The two places Windows itself records that it needs a restart. PendingFileRenameOperations is left out
// on purpose: installers and antivirus leave it set for days, and a wrong "reboot required" costs
// somebody a maintenance window they did not need.
var windowsRebootMarkers = []struct{ path, reason string }{
	{`SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired`, "windows_update"},
	{`SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending`, "component_servicing"},
}

func systemWindows() System {
	var s System

	const current = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	name := regString(current, "ProductName")
	if v := regString(current, "DisplayVersion"); v != "" && name != "" {
		name += " " + v
	}
	s.OS = clip(name, maxSystemField)

	if build := regString(current, "CurrentBuildNumber"); build != "" {
		kernel := "10.0." + build
		if ubr, ok := regDword(current, "UBR"); ok {
			kernel += "." + itoa(int(ubr))
		}
		s.Kernel = clip(kernel, maxSystemField)
	}

	for _, m := range windowsRebootMarkers {
		if regKeyExists(m.path) {
			s.RebootRequired, s.RebootReason = true, m.reason

			break
		}
	}

	s.CPUs = runtime.NumCPU()

	return s
}

func openKey(path string) (syscall.Handle, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var h syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_LOCAL_MACHINE, p, 0, syscall.KEY_READ|keyWow6464Key, &h) != nil {
		return 0, false
	}

	return h, true
}

func regKeyExists(path string) bool {
	h, ok := openKey(path)
	if ok {
		_ = syscall.RegCloseKey(h)
	}

	return ok
}

func regValue(path, name string) ([]byte, uint32, bool) {
	h, ok := openKey(path)
	if !ok {
		return nil, 0, false
	}
	defer func() { _ = syscall.RegCloseKey(h) }()

	n, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, 0, false
	}
	var typ, size uint32
	if syscall.RegQueryValueEx(h, n, nil, &typ, nil, &size) != nil || size == 0 || size > 4096 {
		return nil, 0, false
	}
	buf := make([]byte, size)
	if syscall.RegQueryValueEx(h, n, nil, &typ, &buf[0], &size) != nil {
		return nil, 0, false
	}

	return buf[:size], typ, true
}

func regString(path, name string) string {
	buf, typ, ok := regValue(path, name)
	if !ok || typ != syscall.REG_SZ || len(buf) < 2 {
		return ""
	}

	return syscall.UTF16ToString(unsafe.Slice((*uint16)(unsafe.Pointer(&buf[0])), len(buf)/2))
}

func regDword(path, name string) (uint32, bool) {
	buf, typ, ok := regValue(path, name)
	if !ok || typ != syscall.REG_DWORD || len(buf) < 4 {
		return 0, false
	}

	return *(*uint32)(unsafe.Pointer(&buf[0])), true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}

	return string(b[i:])
}

// The layouts EnumServicesStatusExW and QueryServiceConfigW fill in.
type serviceStatusProcess struct {
	ServiceType, CurrentState, ControlsAccepted, Win32ExitCode, ServiceSpecificExitCode uint32
	CheckPoint, WaitHint, ProcessID, ServiceFlags                                       uint32
}

type enumServiceStatusProcess struct {
	ServiceName *uint16
	DisplayName *uint16
	Status      serviceStatusProcess
}

type queryServiceConfig struct {
	ServiceType, StartType, ErrorControl uint32
	BinaryPathName, LoadOrderGroup       *uint16
	TagID                                uint32
	Dependencies, ServiceStartName       *uint16
	DisplayName                          *uint16
}

// servicesWindows lists the automatic-start services and how each is doing, in the shape the systemd
// collector uses: `active` running, `failed` stopped with an error, `inactive` stopped cleanly — many
// automatic services stop on purpose once their work is done, and only an error exit is a failure.
func servicesWindows() ([]Service, error) {
	scm, _, err := procOpenSCManager.Call(0, 0, scManagerEnumerate)
	if scm == 0 {
		return nil, err
	}
	defer func() { _, _, _ = procCloseServiceHandle.Call(scm) }()

	var out []Service
	var resume uint32
	buf := make([]byte, 64<<10)

	for {
		var needed, returned uint32
		r, _, callErr := procEnumServicesStatusEx.Call(scm, scEnumProcessInfo, serviceWin32, serviceStateAll,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
			uintptr(unsafe.Pointer(&needed)), uintptr(unsafe.Pointer(&returned)), uintptr(unsafe.Pointer(&resume)), 0)
		more := r == 0 && callErr == syscall.Errno(errorMoreData)
		if r == 0 && !more {
			return nil, callErr
		}

		entries := unsafe.Slice((*enumServiceStatusProcess)(unsafe.Pointer(&buf[0])), returned)
		for _, e := range entries {
			if len(out) >= maxWindowsServices {
				return out, nil
			}
			name := utf16PtrToString(e.ServiceName)
			if serviceStartType(scm, e.ServiceName) != serviceAutoStart {
				continue
			}
			out = append(out, windowsService(name, utf16PtrToString(e.DisplayName), e.Status))
		}

		if !more {
			return out, nil
		}
		if int(needed) > len(buf) {
			buf = make([]byte, needed)
		}
	}
}

func windowsService(name, display string, st serviceStatusProcess) Service {
	svc := Service{Unit: clip(name, maxUnit), Description: clip(display, maxDescription)}

	switch st.CurrentState {
	case serviceRunning:
		svc.ActiveState, svc.SubState = "active", "running"
	case serviceStartPending:
		svc.ActiveState, svc.SubState = "activating", "start-pending"
	case serviceStopPending:
		svc.ActiveState, svc.SubState = "deactivating", "stop-pending"
	case serviceStopped:
		svc.SubState = "stopped"
		if st.Win32ExitCode != 0 && st.Win32ExitCode != errorServiceNeverStarted {
			code := int32(st.Win32ExitCode)
			svc.ActiveState, svc.Result, svc.ExecMainStatus = "failed", "exit-code", &code
		} else {
			svc.ActiveState = "inactive"
		}
	default:
		svc.ActiveState, svc.SubState = "inactive", "paused"
	}

	return svc
}

func serviceStartType(scm uintptr, name *uint16) uint32 {
	h, _, _ := procOpenService.Call(scm, uintptr(unsafe.Pointer(name)), serviceQueryConfig)
	if h == 0 {
		return 0
	}
	defer func() { _, _, _ = procCloseServiceHandle.Call(h) }()

	var needed uint32
	_, _, err := procQueryServiceConfig.Call(h, 0, 0, uintptr(unsafe.Pointer(&needed)))
	if err != syscall.Errno(errorInsufficientBuffer) || needed == 0 || needed > 64<<10 {
		return 0
	}
	buf := make([]byte, needed)
	r, _, _ := procQueryServiceConfig.Call(h, uintptr(unsafe.Pointer(&buf[0])), uintptr(needed), uintptr(unsafe.Pointer(&needed)))
	if r == 0 {
		return 0
	}

	return (*queryServiceConfig)(unsafe.Pointer(&buf[0])).StartType
}

func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0 && n < 512; n++ {
		ptr = unsafe.Add(ptr, 2)
	}

	return syscall.UTF16ToString(unsafe.Slice(p, n))
}
