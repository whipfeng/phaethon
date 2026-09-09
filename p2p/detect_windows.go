package p2p

import (
	"syscall"
	"unsafe"
)

var (
	ntdll                   = syscall.NewLazyDLL("ntdll.dll")
	procRtlGetVersion       = ntdll.NewProc("RtlGetVersion")
)

// osVersionInfoEx matches the OSVERSIONINFOEXW struct.
type osVersionInfoEx struct {
	osVersionInfoSize uint32
	majorVersion      uint32
	minorVersion      uint32
	buildNumber       uint32
	platformId        uint32
	csdVersion        [128]uint16
	servicePackMajor  uint16
	servicePackMinor  uint16
	suiteMask         uint16
	productType       byte
	reserved          byte
}

// DetectBuildTag returns a build tag based on the running Windows version.
// Windows 7/8/8.1 (major < 10) returns "win7" to indicate legacy compatibility.
// Windows 10+ returns empty string (standard build).
func DetectBuildTag() string {
	if procRtlGetVersion.Find() != nil {
		return ""
	}

	var info osVersionInfoEx
	info.osVersionInfoSize = uint32(unsafe.Sizeof(info))
	ret, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&info)))
	if ret != 0 {
		return ""
	}

	if info.majorVersion < 10 {
		return "win7"
	}
	return ""
}
