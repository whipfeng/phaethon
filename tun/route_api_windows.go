//go:build windows

package tun

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi = windows.NewLazySystemDLL("iphlpapi.dll")

	procCreateIpForwardEntry2       = modiphlpapi.NewProc("CreateIpForwardEntry2")
	procDeleteIpForwardEntry2       = modiphlpapi.NewProc("DeleteIpForwardEntry2")
	procInitializeIpForwardEntry    = modiphlpapi.NewProc("InitializeIpForwardEntry")
	procCreateUnicastIpAddressEntry = modiphlpapi.NewProc("CreateUnicastIpAddressEntry")
	procDeleteUnicastIpAddressEntry = modiphlpapi.NewProc("DeleteUnicastIpAddressEntry")
	procInitializeUnicastIpAddressEntry = modiphlpapi.NewProc("InitializeUnicastIpAddressEntry")
	procConvertInterfaceAliasToLuid = modiphlpapi.NewProc("ConvertInterfaceAliasToLuid")
	procConvertInterfaceLuidToIndex = modiphlpapi.NewProc("ConvertInterfaceLuidToIndex")
	procGetIpForwardTable2          = modiphlpapi.NewProc("GetIpForwardTable2")
	procFreeMibTable                = modiphlpapi.NewProc("FreeMibTable")
)

// sockaddrInet represents the SOCKADDR_INET union (28 bytes).
type sockaddrInet [28]byte

func newSockaddrInet(ip net.IP) sockaddrInet {
	var s sockaddrInet
	if ip4 := ip.To4(); ip4 != nil {
		*(*uint16)(unsafe.Pointer(&s[0])) = windows.AF_INET
		copy(s[4:8], ip4)
	} else if ip6 := ip.To16(); ip6 != nil {
		*(*uint16)(unsafe.Pointer(&s[0])) = windows.AF_INET6
		copy(s[8:24], ip6)
	}
	return s
}

// mibIpForwardRow2 represents MIB_IPFORWARD_ROW2 (104 bytes on x64).
type mibIpForwardRow2 [104]byte

func (r *mibIpForwardRow2) init() {
	procInitializeIpForwardEntry.Call(uintptr(unsafe.Pointer(&r[0])))
}

func (r *mibIpForwardRow2) setInterfaceLuid(luid uint64) {
	*(*uint64)(unsafe.Pointer(&r[0])) = luid
}

func (r *mibIpForwardRow2) setInterfaceIndex(index uint32) {
	*(*uint32)(unsafe.Pointer(&r[8])) = index
}

func (r *mibIpForwardRow2) setDestinationPrefix(ip net.IP, prefixLen uint8) {
	s := newSockaddrInet(ip)
	copy(r[12:40], s[:])
	r[40] = prefixLen
}

func (r *mibIpForwardRow2) setNextHop(ip net.IP) {
	s := newSockaddrInet(ip)
	copy(r[44:72], s[:])
}

func (r *mibIpForwardRow2) setMetric(metric uint32) {
	*(*uint32)(unsafe.Pointer(&r[84])) = metric
}

func (r *mibIpForwardRow2) isDefaultRoute() bool {
	if r[40] != 0 {
		return false
	}
	family := *(*uint16)(unsafe.Pointer(&r[12]))
	if family != windows.AF_INET {
		return false
	}
	for i := 16; i < 20; i++ {
		if r[i] != 0 {
			return false
		}
	}
	return true
}

func (r *mibIpForwardRow2) nextHop() net.IP {
	family := *(*uint16)(unsafe.Pointer(&r[44]))
	if family == windows.AF_INET {
		return net.IP(r[48:52])
	}
	return nil
}

// mibUnicastIpAddressRow represents MIB_UNICASTIPADDRESS_ROW (80 bytes on x64).
type mibUnicastIpAddressRow [80]byte

func (r *mibUnicastIpAddressRow) init() {
	procInitializeUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&r[0])))
}

func (r *mibUnicastIpAddressRow) setAddress(ip net.IP) {
	s := newSockaddrInet(ip)
	copy(r[0:28], s[:])
}

func (r *mibUnicastIpAddressRow) setInterfaceLuid(luid uint64) {
	*(*uint64)(unsafe.Pointer(&r[28])) = luid
}

func (r *mibUnicastIpAddressRow) setInterfaceIndex(index uint32) {
	*(*uint32)(unsafe.Pointer(&r[36])) = index
}

func (r *mibUnicastIpAddressRow) setOnLinkPrefixLength(length uint8) {
	r[56] = length
}

func getDefaultGatewayWindows() (net.IP, uint64, uint32, error) {
	out, err := exec.Command("netsh", "interface", "ip", "show", "route").Output()
	if err != nil {
		return nil, 0, 0, fmt.Errorf("netsh show route: %w", err)
	}

	var gw net.IP
	var idx uint32
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// netsh output columns (Chinese/English): publish, type, metric, prefix, idx, gateway
		// The prefix field is the 4th field (index 3 in zero-based Fields).
		if fields[3] != "0.0.0.0/0" {
			continue
		}
		// Interface index is the 5th field.
		if _, err := fmt.Sscanf(fields[4], "%d", &idx); err != nil {
			continue
		}
		// Gateway is the 6th field.
		gw = net.ParseIP(fields[5])
		break
	}

	if gw == nil {
		return nil, 0, 0, fmt.Errorf("no default gateway found")
	}

	iface, err := net.InterfaceByIndex(int(idx))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("lookup interface %d: %w", idx, err)
	}
	luid, _, err := getInterfaceLUID(iface.Name)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("lookup luid for %s: %w", iface.Name, err)
	}
	return gw.To4(), luid, idx, nil
}

func getInterfaceLUID(name string) (uint64, uint32, error) {
	alias, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, 0, err
	}
	var luid uint64
	ret, _, _ := procConvertInterfaceAliasToLuid.Call(
		uintptr(unsafe.Pointer(alias)),
		uintptr(unsafe.Pointer(&luid)),
	)
	if ret != 0 {
		return 0, 0, fmt.Errorf("ConvertInterfaceAliasToLuid: 0x%x", ret)
	}
	var index uint32
	ret, _, _ = procConvertInterfaceLuidToIndex.Call(
		uintptr(unsafe.Pointer(&luid)),
		uintptr(unsafe.Pointer(&index)),
	)
	if ret != 0 {
		return 0, 0, fmt.Errorf("ConvertInterfaceLuidToIndex: 0x%x", ret)
	}
	return luid, index, nil
}
