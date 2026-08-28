package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi                   = windows.NewLazySystemDLL("iphlpapi.dll")
	procConvertInterfaceAliasToLuid = modiphlpapi.NewProc("ConvertInterfaceAliasToLuid")
	procGetIfEntry2               = modiphlpapi.NewProc("GetIfEntry2")
	procSetIfEntry2               = modiphlpapi.NewProc("SetIfEntry2")
)

// MIB_IF_ROW2 - we only need the first few fields
// Actual size is ~1360 bytes but we'll use a smaller buffer
type mibIfRow2 [1400]byte

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
	return luid, 0, nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: iftype <get|set-p2p>")
		os.Exit(1)
	}

	luid, _, err := getInterfaceLUID("phaethontun")
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Interface LUID: 0x%x\n\n", luid)

	var row mibIfRow2
	// Set InterfaceLuid at offset 0
	*(*uint64)(unsafe.Pointer(&row[0])) = luid

	ret, _, _ := procGetIfEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
	if ret != 0 {
		fmt.Printf("GetIfEntry2 failed: 0x%x\n", ret)
		os.Exit(1)
	}

	// Parse key fields
	// InterfaceIndex at offset 8
	ifIndex := *(*uint32)(unsafe.Pointer(&row[8]))
	fmt.Printf("InterfaceIndex: %d\n", ifIndex)

	// InterfaceName at offset 12 (WCHAR[256])
	nameBytes := row[12:524]
	name := windows.UTF16PtrToString((*uint16)(unsafe.Pointer(&nameBytes[0])))
	fmt.Printf("InterfaceName: %s\n", name)

	// Scan for important fields
	fmt.Println("\n=== Scanning for interface type fields ===")
	
	// Look for Type (IF_TYPE) - should be around offset 1036-1100
	// IF_TYPE_ETHERNET_CSMACD = 6
	// IF_TYPE_PROPVIRTUAL = 53
	// IF_TYPE_TUNNEL = 94
	
	for offset := 1030; offset < 1150; offset += 4 {
		val := *(*uint32)(unsafe.Pointer(&row[offset]))
		if val > 0 && val < 200 {
			fmt.Printf("  offset %d: %d\n", offset, val)
		}
	}

	// Also check for ConnectionType and other fields
	fmt.Println("\n=== Looking for ConnectionType/MediaType ===")
	// ConnectionType: 1 = network, 2 = point-to-point
	// Let's search for small values
	for offset := 1050; offset < 1200; offset += 4 {
		val := *(*uint32)(unsafe.Pointer(&row[offset]))
		if val == 1 || val == 2 {
			fmt.Printf("  offset %d: %d (possible ConnectionType)\n", offset, val)
		}
	}

	if os.Args[1] == "set-p2p" {
		fmt.Println("\n=== Attempting to set ConnectionType to point-to-point ===")
		
		// Try setting offset 1060 to 2 (point-to-point)
		// This is a guess based on typical MIB_IF_ROW2 layout
		fmt.Println("Trying to set ConnectionType=2 at offset 1060...")
		*(*uint32)(unsafe.Pointer(&row[1060])) = 2
		
		ret, _, _ = procSetIfEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
		if ret != 0 {
			fmt.Printf("SetIfEntry2 failed: 0x%x\n", ret)
		} else {
			fmt.Println("SUCCESS: SetIfEntry2 completed")
		}
	}
}
