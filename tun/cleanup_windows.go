//go:build windows

package tun

import (
	"net"
	"strings"
	"time"
	"unsafe"

	"phaethon/util"
)

// CleanupResidual removes leftover TUN adapter and routes from a previous crash.
func CleanupResidual() {
	util.LogModuleInfo("cleanup", "[DIAG] step 1: getInterfaceLUID")
	luid, index, err := getInterfaceLUID("phaethontun")
	if err == nil {
		util.LogModuleInfo("cleanup", "[DIAG] step 2: interface exists, deleting split-tunnel routes")
		// Delete split-tunnel routes (0.0.0.0/1 and 128.0.0.0/1) with on-link next hop.
		for _, prefix := range []struct {
			ip  net.IP
			len uint8
		}{
			{net.ParseIP("0.0.0.0").To4(), 1},
			{net.ParseIP("128.0.0.0").To4(), 1},
		} {
			var fwdRow mibIpForwardRow2
			fwdRow.init()
			fwdRow.setInterfaceLuid(luid)
			fwdRow.setInterfaceIndex(index)
			fwdRow.setDestinationPrefix(prefix.ip, prefix.len)
			fwdRow.setNextHop(net.IPv4zero)
			fwdRow.setMetric(1)
			procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&fwdRow[0])))
		}

		// Sweep the full route table for residual TUN routes that may not match
		// the specific combinations above (e.g. persistent routes detached from
		// the adapter LUID).
		util.LogModuleInfo("cleanup", "[DIAG] step 3: deleteResidualRoutesAPI")
		deleteResidualRoutesAPI()

		// Remove residual exclusion routes left by a previous abnormal exit.
		// These routes point at the original physical gateway with metric 1 and
		// would otherwise make the next Setup() fail with ERROR_OBJECT_ALREADY_EXISTS.
		util.LogModuleInfo("cleanup", "[DIAG] step 4: getDefaultGatewayWindows")
		if gw, gwLuid, gwIdx, err := getDefaultGatewayWindows(); err == nil {
			util.LogModuleInfo("cleanup", "[DIAG] step 5: deleteResidualExclusionRoute loop")
			for _, exclude := range DefaultLANExclusions {
				deleteResidualExclusionRoute(gwLuid, gwIdx, gw, exclude)
			}
		}

		// Delete interface addresses (mesh-derived addresses).
		// The actual TUN IP is set dynamically, so we clear all IPs from the interface.
		util.LogModuleInfo("cleanup", "[DIAG] step 6: clearInterfaceIPAPI")
		_ = clearInterfaceIPAPI(luid, index)
	}

	// Check if phaethontun adapter still exists and remove it. Disabling the
	// adapter leaves a dead Wintun interface in the system that prevents
	// subsequent CreateAdapter calls from creating a fresh adapter, so we
	// explicitly delete it.
	util.LogModuleInfo("cleanup", "[DIAG] step 7: InterfaceExists check")
	if InterfaceExists() {
		util.LogModuleInfo("cleanup", "[DIAG] step 8: interface exists, clearing IP/DNS")
		// Release any static IP/DNS first so the removal is clean.
		if luid, index, err := getInterfaceLUID("phaethontun"); err == nil {
			_ = clearInterfaceIPAPI(luid, index)
			_ = clearInterfaceDNSAPI(luid, index)
		}

		util.LogModuleInfo("cleanup", "removing orphaned adapter phaethontun")
		util.LogModuleInfo("cleanup", "[DIAG] step 9: removing orphaned adapter")
		adapterRemoved := false
		if err := removeAdapterAPI("phaethontun"); err != nil {
			util.LogModuleWarn("cleanup", "remove adapter failed: %v", err)
			// If adapter is not found in device list, consider it already removed
			// and skip the wait loop to avoid hanging.
			if strings.Contains(err.Error(), "not found") {
				adapterRemoved = true
			} else {
				if err2 := disableInterfaceAPI("phaethontun"); err2 != nil {
					util.LogModuleWarn("cleanup", "disable adapter fallback failed: %v", err2)
				}
			}
		} else {
			adapterRemoved = true
		}

		// Wait until the adapter is actually gone; CreateAdapter can fail or
		// attach to a stale adapter if we proceed too quickly.
		// Skip wait if adapter was already reported as not found.
		if !adapterRemoved {
			util.LogModuleInfo("cleanup", "[DIAG] step 10: waiting for adapter removal")
			for i := 0; i < 150; i++ {
				if _, _, err := getInterfaceLUID("phaethontun"); err != nil {
					util.LogModuleInfo("cleanup", "[DIAG] step 11: adapter removed after %d iterations", i)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	// Reset physical interface DNS to DHCP as a crash-recovery best effort.
	// The original DNS backup is only available during normal Stop(); after a
	// crash we restore DHCP so the machine does not remain stuck on the TUN DNS IP.
	util.LogModuleInfo("cleanup", "[DIAG] step 12: restore DNS for default gateway")
	if ifaceName := defaultGatewayInterfaceName(); ifaceName != "" {
		if gwLuid, _, err := getInterfaceLUID(ifaceName); err == nil {
			if gwIdx, err2 := luidToIndex(gwLuid); err2 == nil {
				if err3 := clearInterfaceDNSAPI(gwLuid, gwIdx); err3 != nil {
					util.LogModuleWarn("cleanup", "restore dns for %s fail: %v", ifaceName, err3)
				} else {
					util.LogModuleInfo("cleanup", "restored dns for %s to dhcp", ifaceName)
				}
			}
		}
	}

	util.LogModuleInfo("cleanup", "residual cleanup completed")
}

// defaultGatewayInterfaceName returns the interface name of the IPv4 default
// route, or an empty string if it cannot be determined.
func defaultGatewayInterfaceName() string {
	_, _, idx, err := getDefaultGatewayAPI()
	if err != nil {
		return ""
	}
	iface, err := net.InterfaceByIndex(int(idx))
	if err != nil {
		return ""
	}
	return iface.Name
}

// InterfaceExists reports whether the phaethontun network adapter is currently
// present in the TCP/IP stack.
func InterfaceExists() bool {
	_, _, err := getInterfaceLUID("phaethontun")
	return err == nil
}
