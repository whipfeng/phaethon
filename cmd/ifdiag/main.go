package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi = windows.NewLazySystemDLL("iphlpapi.dll")

	procConvertInterfaceAliasToLuid = modiphlpapi.NewProc("ConvertInterfaceAliasToLuid")
	procConvertInterfaceLuidToIndex = modiphlpapi.NewProc("ConvertInterfaceLuidToIndex")
	procCreateIpNetEntry2           = modiphlpapi.NewProc("CreateIpNetEntry2")
	procDeleteIpNetEntry2           = modiphlpapi.NewProc("DeleteIpNetEntry2")
	procCreateIpForwardEntry2       = modiphlpapi.NewProc("CreateIpForwardEntry2")
	procDeleteIpForwardEntry2       = modiphlpapi.NewProc("DeleteIpForwardEntry2")
	procInitializeIpForwardEntry    = modiphlpapi.NewProc("InitializeIpForwardEntry")
)

// SOCKADDR_INET union (28 bytes)
type sockaddrInet [28]byte

func newSockaddrInet(ip net.IP) sockaddrInet {
	var s sockaddrInet
	if ip4 := ip.To4(); ip4 != nil {
		*(*uint16)(unsafe.Pointer(&s[0])) = windows.AF_INET
		copy(s[4:8], ip4)
	}
	return s
}

// MIB_IPNET_ROW2 layout (x64):
// +0:  Address (SOCKADDR_INET, 28 bytes)
// +28: InterfaceIndex (4 bytes)
// +32: InterfaceLuid (8 bytes)
// +40: PhysicalAddress[32] (32 bytes)
// +72: PhysicalAddressLength (1 byte)
// +73: padding (3 bytes)
// +76: State (NL_NEIGHBOR_STATE, 4 bytes) - NLNS_REACHABLE=3, NLNS_PERMANENT=4
// Total: ~80+ bytes, use 104 to be safe
type mibIpNetRow2 [104]byte

func (r *mibIpNetRow2) setAddress(ip net.IP) {
	addr := newSockaddrInet(ip)
	copy(r[0:28], addr[:])
}

func (r *mibIpNetRow2) setInterfaceIndex(index uint32) {
	*(*uint32)(unsafe.Pointer(&r[28])) = index
}

func (r *mibIpNetRow2) setInterfaceLuid(luid uint64) {
	*(*uint64)(unsafe.Pointer(&r[32])) = luid
}

func (r *mibIpNetRow2) setPhysicalAddress(mac []byte) {
	copy(r[40:46], mac)
	r[72] = byte(len(mac))
}

func (r *mibIpNetRow2) setState(state byte) {
	*(*uint32)(unsafe.Pointer(&r[76])) = uint32(state)
}

// MIB_IPFORWARD_ROW2 (104 bytes on x64)
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

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ifdiag <command> [args]")
		fmt.Println("Commands:")
		fmt.Println("  add-neighbor <ip> [aa:bb:cc:dd:ee:ff]  - Add permanent neighbor entry")
		fmt.Println("  del-neighbor <ip>                       - Delete neighbor entry")
		fmt.Println("  add-route <dst>/<len> <gateway>         - Add route")
		fmt.Println("  del-route <dst>/<len> <gateway>         - Delete route")
		fmt.Println("  show-neighbors                          - Show neighbor table")
		fmt.Println("  show-routes                             - Show routes")
		fmt.Println("  fix                                     - Apply the full fix")
		os.Exit(1)
	}

	luid, index, err := getInterfaceLUID("phaethontun")
	if err != nil {
		fmt.Printf("ERROR: getInterfaceLUID: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Interface: phaethontun, LUID=0x%x, Index=%d\n\n", luid, index)

	switch os.Args[1] {
	case "add-neighbor":
		cmdAddNeighbor(luid, index)
	case "del-neighbor":
		cmdDelNeighbor(luid, index)
	case "add-route":
		cmdAddRoute(luid, index)
	case "del-route":
		cmdDelRoute(luid, index)
	case "show-neighbors":
		cmdShowNeighbors()
	case "show-routes":
		cmdShowRoutes()
	case "fix":
		cmdFix(luid, index)
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
	}
}

func cmdAddNeighbor(luid uint64, index uint32) {
	if len(os.Args) < 3 {
		fmt.Println("Usage: ifdiag add-neighbor <ip> [aa:bb:cc:dd:ee:ff]")
		return
	}

	ip := net.ParseIP(os.Args[2]).To4()
	if ip == nil {
		fmt.Printf("Invalid IP: %s\n", os.Args[2])
		return
	}

	var mac [6]byte
	if len(os.Args) >= 4 {
		n, _ := fmt.Sscanf(os.Args[3], "%02x:%02x:%02x:%02x:%02x:%02x",
			&mac[0], &mac[1], &mac[2], &mac[3], &mac[4], &mac[5])
		if n != 6 {
			fmt.Printf("Invalid MAC: %s\n", os.Args[3])
			return
		}
	}

	var row mibIpNetRow2
	row.setAddress(ip)
	row.setInterfaceIndex(index)
	row.setInterfaceLuid(luid)
	row.setPhysicalAddress(mac[:])
	row.setState(4) // NL_ENTRY_PERMANENT

	fmt.Printf("Adding neighbor %s with MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
		ip, mac[0], mac[1], mac[2], mac[3], mac[4], mac[5])
	fmt.Printf("  Row layout: addr[0:28]=%x idx[28:32]=%x luid[32:40]=%x mac[40:46]=%x macLen[72]=%d state[76:80]=%x\n",
		row[0:4], row[28:32], row[32:40], row[40:46], row[72], row[76:80])

	ret, _, _ := procCreateIpNetEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
	if ret != 0 {
		fmt.Printf("CreateIpNetEntry2 failed: 0x%x\n", ret)
	} else {
		fmt.Println("SUCCESS: neighbor entry added")
	}
}

func cmdDelNeighbor(luid uint64, index uint32) {
	if len(os.Args) < 3 {
		fmt.Println("Usage: ifdiag del-neighbor <ip>")
		return
	}

	ip := net.ParseIP(os.Args[2]).To4()
	if ip == nil {
		fmt.Printf("Invalid IP: %s\n", os.Args[2])
		return
	}

	var row mibIpNetRow2
	row.setAddress(ip)
	row.setInterfaceIndex(index)
	row.setInterfaceLuid(luid)

	ret, _, _ := procDeleteIpNetEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
	if ret != 0 {
		fmt.Printf("DeleteIpNetEntry2 failed: 0x%x\n", ret)
	} else {
		fmt.Println("SUCCESS: neighbor entry deleted")
	}
}

func cmdAddRoute(luid uint64, index uint32) {
	if len(os.Args) < 4 {
		fmt.Println("Usage: ifdiag add-route <dst/prefix> <gateway>")
		fmt.Println("Example: ifdiag add-route 0.0.0.0/1 192.0.2.1")
		return
	}

	var dstIP net.IP
	var prefixLen uint8
	_, dstNet, err := net.ParseCIDR(os.Args[2])
	if err != nil {
		fmt.Printf("Invalid CIDR: %s\n", os.Args[2])
		return
	}
	dstIP = dstNet.IP
	ones, _ := dstNet.Mask.Size()
	prefixLen = uint8(ones)

	gw := net.ParseIP(os.Args[3]).To4()
	if gw == nil {
		fmt.Printf("Invalid gateway: %s\n", os.Args[3])
		return
	}

	var row mibIpForwardRow2
	row.init()
	row.setInterfaceLuid(luid)
	row.setInterfaceIndex(index)
	row.setDestinationPrefix(dstIP, prefixLen)
	row.setNextHop(gw)
	row.setMetric(1)

	fmt.Printf("Adding route %s/%d via %s\n", dstIP, prefixLen, gw)
	ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
	if ret != 0 {
		fmt.Printf("CreateIpForwardEntry2 failed: 0x%x\n", ret)
	} else {
		fmt.Println("SUCCESS: route added")
	}
}

func cmdDelRoute(luid uint64, index uint32) {
	if len(os.Args) < 4 {
		fmt.Println("Usage: ifdiag del-route <dst/prefix> <gateway>")
		return
	}

	_, dstNet, err := net.ParseCIDR(os.Args[2])
	if err != nil {
		fmt.Printf("Invalid CIDR: %s\n", os.Args[2])
		return
	}
	dstIP := dstNet.IP
	ones, _ := dstNet.Mask.Size()

	gw := net.ParseIP(os.Args[3]).To4()
	if gw == nil {
		fmt.Printf("Invalid gateway: %s\n", os.Args[3])
		return
	}

	var row mibIpForwardRow2
	row.init()
	row.setInterfaceLuid(luid)
	row.setInterfaceIndex(index)
	row.setDestinationPrefix(dstIP, uint8(ones))
	row.setNextHop(gw)
	row.setMetric(1)

	fmt.Printf("Deleting route %s/%d via %s\n", dstIP, ones, gw)
	ret, _, _ := procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
	if ret != 0 {
		fmt.Printf("DeleteIpForwardEntry2 failed: 0x%x\n", ret)
	} else {
		fmt.Println("SUCCESS: route deleted")
	}
}

func cmdShowNeighbors() {
	fmt.Println("=== netsh neighbors ===")
	out, _ := exec.Command("netsh", "interface", "ipv4", "show", "neighbors", "interface=phaethontun").CombinedOutput()
	fmt.Println(string(out))
}

func cmdShowRoutes() {
	fmt.Println("=== All routes ===")
	out, _ := exec.Command("route", "print", "0.*").CombinedOutput()
	fmt.Println(string(out))
}

func cmdFix(luid uint64, index uint32) {
	fmt.Println("=== Applying fix ===")
	fmt.Println("Strategy: Add permanent neighbor for 192.0.2.1, then switch routes to off-link")

	// Step 1: Add permanent neighbor for 192.0.2.1
	fmt.Println("\n--- Step 1: Add permanent neighbor 192.0.2.1 ---")
	{
		var row mibIpNetRow2
		row.setAddress(net.ParseIP("192.0.2.1").To4())
		row.setInterfaceIndex(index)
		row.setInterfaceLuid(luid)
		row.setPhysicalAddress([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		row.setState(4) // NL_ENTRY_PERMANENT

		ret, _, _ := procCreateIpNetEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
		if ret != 0 {
			fmt.Printf("  FAILED: 0x%x\n", ret)
			fmt.Println("  Trying with non-zero MAC...")

			row.setPhysicalAddress([]byte{0x00, 0x00, 0x5e, 0x00, 0x53, 0x01})
			ret, _, _ = procCreateIpNetEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
			if ret != 0 {
				fmt.Printf("  FAILED again: 0x%x\n", ret)
				return
			}
		}
		fmt.Println("  SUCCESS")
	}

	// Step 2: Verify neighbor
	fmt.Println("\n--- Step 2: Verify neighbor table ---")
	cmdShowNeighbors()

	// Step 3: Delete existing on-link routes
	fmt.Println("\n--- Step 3: Delete existing on-link split-tunnel routes ---")
	for _, prefix := range []struct {
		ip  net.IP
		len uint8
	}{
		{net.ParseIP("0.0.0.0").To4(), 1},
		{net.ParseIP("128.0.0.0").To4(), 1},
	} {
		var row mibIpForwardRow2
		row.init()
		row.setInterfaceLuid(luid)
		row.setInterfaceIndex(index)
		row.setDestinationPrefix(prefix.ip, prefix.len)
		row.setNextHop(net.IPv4zero)
		row.setMetric(1)
		ret, _, _ := procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
		fmt.Printf("  Delete %s/%d via 0.0.0.0: 0x%x\n", prefix.ip, prefix.len, ret)
	}

	// Step 4: Add off-link routes via 192.0.2.1
	fmt.Println("\n--- Step 4: Add off-link split-tunnel routes via 192.0.2.1 ---")
	for _, prefix := range []struct {
		ip  net.IP
		len uint8
	}{
		{net.ParseIP("0.0.0.0").To4(), 1},
		{net.ParseIP("128.0.0.0").To4(), 1},
	} {
		var row mibIpForwardRow2
		row.init()
		row.setInterfaceLuid(luid)
		row.setInterfaceIndex(index)
		row.setDestinationPrefix(prefix.ip, prefix.len)
		row.setNextHop(net.ParseIP("192.0.2.1").To4())
		row.setMetric(1)
		ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
		if ret != 0 {
			fmt.Printf("  FAILED %s/%d via 192.0.2.1: 0x%x\n", prefix.ip, prefix.len, ret)
		} else {
			fmt.Printf("  SUCCESS %s/%d via 192.0.2.1\n", prefix.ip, prefix.len)
		}
	}

	// Step 5: Also fix Fake-IP route
	fmt.Println("\n--- Step 5: Fix Fake-IP pool route ---")
	{
		// Delete old on-link
		_, fakeIPNet, _ := net.ParseCIDR("198.18.0.0/15")
		var delRow mibIpForwardRow2
		delRow.init()
		delRow.setInterfaceLuid(luid)
		delRow.setInterfaceIndex(index)
		delRow.setDestinationPrefix(fakeIPNet.IP, 15)
		delRow.setNextHop(net.IPv4zero)
		delRow.setMetric(1)
		procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(&delRow[0])))

		// Add off-link
		var addRow mibIpForwardRow2
		addRow.init()
		addRow.setInterfaceLuid(luid)
		addRow.setInterfaceIndex(index)
		addRow.setDestinationPrefix(fakeIPNet.IP, 15)
		addRow.setNextHop(net.ParseIP("192.0.2.1").To4())
		addRow.setMetric(1)
		ret, _, _ := procCreateIpForwardEntry2.Call(uintptr(unsafe.Pointer(&addRow[0])))
		fmt.Printf("  Fake-IP route via 192.0.2.1: 0x%x\n", ret)
	}

	// Step 6: Test
	fmt.Println("\n--- Step 6: Test ping ---")
	out, _ := exec.Command("ping", "-n", "2", "-w", "3000", "1.1.1.1").CombinedOutput()
	fmt.Println(string(out))

	fmt.Println("\n--- Neighbor table after ping ---")
	cmdShowNeighbors()
}
