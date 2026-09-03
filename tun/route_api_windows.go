//go:build windows

package tun

import (
	"fmt"
	"net"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi = windows.NewLazySystemDLL("iphlpapi.dll")
	modsetupapi = windows.NewLazySystemDLL("setupapi.dll")
	moddnsapi   = windows.NewLazySystemDLL("dnsapi.dll")

	procCreateIpForwardEntry2             = modiphlpapi.NewProc("CreateIpForwardEntry2")
	procDeleteIpForwardEntry2             = modiphlpapi.NewProc("DeleteIpForwardEntry2")
	procInitializeIpForwardEntry          = modiphlpapi.NewProc("InitializeIpForwardEntry")
	procCreateUnicastIpAddressEntry       = modiphlpapi.NewProc("CreateUnicastIpAddressEntry")
	procDeleteUnicastIpAddressEntry       = modiphlpapi.NewProc("DeleteUnicastIpAddressEntry")
	procInitializeUnicastIpAddressEntry   = modiphlpapi.NewProc("InitializeUnicastIpAddressEntry")
	procConvertInterfaceAliasToLuid       = modiphlpapi.NewProc("ConvertInterfaceAliasToLuid")
	procConvertInterfaceLuidToIndex       = modiphlpapi.NewProc("ConvertInterfaceLuidToIndex")
	procGetIpForwardTable2                = modiphlpapi.NewProc("GetIpForwardTable2")
	procFreeMibTable                      = modiphlpapi.NewProc("FreeMibTable")
	procGetBestRoute2                     = modiphlpapi.NewProc("GetBestRoute2")
	procGetIpInterfaceEntry               = modiphlpapi.NewProc("GetIpInterfaceEntry")
	procSetIpInterfaceEntry               = modiphlpapi.NewProc("SetIpInterfaceEntry")
	procInitializeIpInterfaceEntry        = modiphlpapi.NewProc("InitializeIpInterfaceEntry")
	procGetIfEntry2                       = modiphlpapi.NewProc("GetIfEntry2")
	procSetIfEntry                        = modiphlpapi.NewProc("SetIfEntry")
	procDeleteIpNetEntry2                 = modiphlpapi.NewProc("DeleteIpNetEntry2")
	procGetIpNetTable2                    = modiphlpapi.NewProc("GetIpNetTable2")
	procGetUnicastIpAddressTable          = modiphlpapi.NewProc("GetUnicastIpAddressTable")
	procSetInterfaceDnsSettings            = modiphlpapi.NewProc("SetInterfaceDnsSettings")

	procSetupDiGetClassDevsW              = modsetupapi.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInfo             = modsetupapi.NewProc("SetupDiEnumDeviceInfo")
	procSetupDiCallClassInstaller         = modsetupapi.NewProc("SetupDiCallClassInstaller")
	procSetupDiGetDeviceInstanceIdW       = modsetupapi.NewProc("SetupDiGetDeviceInstanceIdW")
	procSetupDiGetDeviceRegistryPropertyW = modsetupapi.NewProc("SetupDiGetDeviceRegistryPropertyW")
	procSetupDiDestroyDeviceInfoList      = modsetupapi.NewProc("SetupDiDestroyDeviceInfoList")

	procDnsFlushResolverCache             = moddnsapi.NewProc("DnsFlushResolverCache")
)

// interfaceDnsSettings represents the INTERFACE_DNS_SETTINGS structure for SetInterfaceDnsSettings.
// See: https://learn.microsoft.com/en-us/windows/win32/api/netioapi/ns-netioapi-interface_dns_settings
type interfaceDnsSettings struct {
	Version        uint32
	Flags          uint32
	DnsServer      *uint16
	PrivateProfile *uint16
	Domain         *uint16
	Nickname       *uint16
	NameServer     *uint16
}

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
	*(*uint64)(unsafe.Pointer(&r[32])) = luid
}

func (r *mibUnicastIpAddressRow) setInterfaceIndex(index uint32) {
	*(*uint32)(unsafe.Pointer(&r[40])) = index
}

func (r *mibUnicastIpAddressRow) setOnLinkPrefixLength(length uint8) {
	r[60] = length
}

func getDefaultGatewayWindows() (net.IP, uint64, uint32, error) {
	return getDefaultGatewayAPI()
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
	index, err := luidToIndex(luid)
	if err != nil {
		return 0, 0, err
	}
	return luid, index, nil
}

// luidToIndex converts an interface LUID to its index.
func luidToIndex(luid uint64) (uint32, error) {
	var index uint32
	ret, _, _ := procConvertInterfaceLuidToIndex.Call(
		uintptr(unsafe.Pointer(&luid)),
		uintptr(unsafe.Pointer(&index)),
	)
	if ret != 0 {
		return 0, fmt.Errorf("ConvertInterfaceLuidToIndex: 0x%x", ret)
	}
	return index, nil
}

// getBestRouteInterface returns the interface index for the best route to dst,
// excluding the interface identified by excludeLuid (the phaethon TUN
// interface). If the best route points back to TUN, it falls back to the
// original default gateway interface.
func getBestRouteInterface(dst net.IP, excludeLuid uint64, fallbackIdx uint32) (uint32, error) {
	dstAddr := newSockaddrInet(dst)
	var bestRoute mibIpForwardRow2
	var bestSrc sockaddrInet

	ret, _, _ := procGetBestRoute2.Call(
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&dstAddr)),
		0,
		uintptr(unsafe.Pointer(&bestRoute[0])),
		uintptr(unsafe.Pointer(&bestSrc)),
	)
	if ret != 0 {
		return 0, fmt.Errorf("GetBestRoute2: 0x%x", ret)
	}

	luid := *(*uint64)(unsafe.Pointer(&bestRoute[0]))
	index := *(*uint32)(unsafe.Pointer(&bestRoute[8]))
	if luid == excludeLuid {
		if fallbackIdx == 0 {
			return 0, fmt.Errorf("best route leads to TUN and no fallback interface")
		}
		return fallbackIdx, nil
	}
	return index, nil
}

// getAdapterDNS returns the IPv4 DNS server addresses for the network adapter
// with the given interface name, using GetAdaptersAddresses.
func getAdapterDNS(ifaceName string) ([]string, error) {
	var size uint32
	const flags = windows.GAA_FLAG_INCLUDE_PREFIX | windows.GAA_FLAG_SKIP_FRIENDLY_NAME | windows.GAA_FLAG_SKIP_MULTICAST
	windows.GetAdaptersAddresses(windows.AF_INET, flags, 0, nil, &size)
	if size == 0 {
		return nil, fmt.Errorf("GetAdaptersAddresses returned zero size")
	}

	buf := make([]byte, size)
	addr := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0]))
	if err := windows.GetAdaptersAddresses(windows.AF_INET, flags, 0, addr, &size); err != nil {
		return nil, fmt.Errorf("GetAdaptersAddresses: %w", err)
	}

	var servers []string
	for ; addr != nil; addr = addr.Next {
		name := windows.UTF16PtrToString(addr.FriendlyName)
		if name != ifaceName {
			continue
		}
		for dns := addr.FirstDnsServerAddress; dns != nil; dns = dns.Next {
			if ip := dns.Address.IP(); ip != nil {
				servers = append(servers, ip.String())
			}
		}
	}

	if len(servers) == 0 {
		return nil, fmt.Errorf("no DNS servers found for interface %s", ifaceName)
	}
	return servers, nil
}

// mibIpNetRow2 represents MIB_IPNET_ROW2 (104 bytes on x64).
// Used for neighbor/ARP table operations.
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

// spDevInfoData represents SP_DEVINFO_DATA (32 bytes on x64).
type spDevInfoData [32]byte

func (d *spDevInfoData) init() {
	*d = spDevInfoData{}
	*(*uint32)(unsafe.Pointer(&d[0])) = 32
}

// GUID_DEVCLASS_NET = {4d36e972-e325-11ce-bfc1-08002be10318}
var guidDevClassNet = windows.GUID{
	Data1: 0x4d36e972,
	Data2: 0xe325,
	Data3: 0x11ce,
	Data4: [8]byte{0xbf, 0xc1, 0x08, 0x00, 0x2b, 0xe1, 0x03, 0x18},
}

// getIpInterfaceEntryAPI calls GetIpInterfaceEntry and returns the populated row.
func getIpInterfaceEntryAPI(luid uint64, index uint32) (*windows.MibIpInterfaceRow, error) {
	var row windows.MibIpInterfaceRow
	row.Family = windows.AF_INET
	row.InterfaceLuid = luid
	row.InterfaceIndex = index
	ret, _, _ := procGetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row)))
	if ret != 0 {
		return nil, fmt.Errorf("GetIpInterfaceEntry: 0x%x", ret)
	}
	return &row, nil
}

// setIpInterfaceEntryAPI calls SetIpInterfaceEntry to write interface settings.
//
// On Windows 11 24H2 (build 26200+), SetIpInterfaceEntry with Family=AF_INET
// always returns ERROR_INVALID_PARAMETER (0x57) regardless of the row contents.
// The workaround is to initialize the row with InitializeIpInterfaceEntry using
// AF_INET6, then switch Family to AF_INET before calling Set. The weak-host
// flags are per-interface (not per-family), so this correctly sets IPv4
// weak-host state.
func setIpInterfaceEntryAPI(luid uint64, index uint32, modify func(*windows.MibIpInterfaceRow)) error {
	var row windows.MibIpInterfaceRow
	row.Family = windows.AF_INET6
	ret, _, _ := procInitializeIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row)))
	if ret != 0 {
		return fmt.Errorf("InitializeIpInterfaceEntry: 0x%x", ret)
	}
	row.Family = windows.AF_INET
	row.InterfaceLuid = luid
	row.InterfaceIndex = index
	modify(&row)
	ret, _, _ = procSetIpInterfaceEntry.Call(uintptr(unsafe.Pointer(&row)))
	if ret != 0 {
		return fmt.Errorf("SetIpInterfaceEntry: 0x%x", ret)
	}
	return nil
}

// getWeakHostStateAPI returns the weak-host send/receive state for the named interface.
func getWeakHostStateAPI(name string) (send, recv bool, err error) {
	luid, index, err := getInterfaceLUID(name)
	if err != nil {
		return false, false, err
	}
	row, err := getIpInterfaceEntryAPI(luid, index)
	if err != nil {
		return false, false, err
	}
	return row.WeakHostSend != 0, row.WeakHostReceive != 0, nil
}

// setWeakHostSendAPI sets weak-host send on the named interface.
func setWeakHostSendAPI(name string, v bool) error {
	luid, index, err := getInterfaceLUID(name)
	if err != nil {
		return err
	}
	val := uint8(0)
	if v {
		val = 1
	}
	return setIpInterfaceEntryAPI(luid, index, func(row *windows.MibIpInterfaceRow) {
		row.WeakHostSend = val
	})
}

// setWeakHostReceiveAPI sets weak-host receive on the named interface.
func setWeakHostReceiveAPI(name string, v bool) error {
	luid, index, err := getInterfaceLUID(name)
	if err != nil {
		return err
	}
	val := uint8(0)
	if v {
		val = 1
	}
	return setIpInterfaceEntryAPI(luid, index, func(row *windows.MibIpInterfaceRow) {
		row.WeakHostReceive = val
	})
}

// setInterfaceMetricAPI sets the interface metric or automatic metric mode.
func setInterfaceMetricAPI(name string, metric uint32, automatic bool) error {
	luid, index, err := getInterfaceLUID(name)
	if err != nil {
		return err
	}
	return setIpInterfaceEntryAPI(luid, index, func(row *windows.MibIpInterfaceRow) {
		if automatic {
			row.UseAutomaticMetric = 1
		} else {
			row.UseAutomaticMetric = 0
			row.Metric = metric
		}
	})
}

// setInterfaceIPAPI sets a static IP address on the interface.
func setInterfaceIPAPI(luid uint64, index uint32, ip net.IP, prefixLen uint8) error {
	var row mibUnicastIpAddressRow
	row.init()
	row.setAddress(ip)
	row.setInterfaceLuid(luid)
	row.setInterfaceIndex(index)
	row.setOnLinkPrefixLength(prefixLen)
	ret, _, _ := procCreateUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&row[0])))
	if ret != 0 && ret != 0x490 { // 0x490 = ERROR_OBJECT_ALREADY_EXISTS
		return fmt.Errorf("CreateUnicastIpAddressEntry: 0x%x", ret)
	}
	return nil
}

// clearInterfaceIPAPI removes all IPv4 addresses from the interface.
func clearInterfaceIPAPI(luid uint64, index uint32) error {
	var table uintptr
	ret, _, _ := procGetUnicastIpAddressTable.Call(
		uintptr(windows.AF_INET),
		uintptr(unsafe.Pointer(&table)),
	)
	if ret != 0 {
		return fmt.Errorf("GetUnicastIpAddressTable: 0x%x", ret)
	}
	defer procFreeMibTable.Call(table)

	if table == 0 {
		return nil
	}

	count := *(*uint32)(unsafe.Pointer(table))
	// MIB_UNICASTIPADDRESS_ROW has uint64 alignment, so the Table array
	// starts at offset 8 (4 bytes NumEntries + 4 bytes padding).
	base := table + 8
	rowSize := uintptr(80)

	for i := uint32(0); i < count; i++ {
		rowPtr := base + uintptr(i)*rowSize
		rowLuid := *(*uint64)(unsafe.Pointer(rowPtr + 32))
		rowIndex := *(*uint32)(unsafe.Pointer(rowPtr + 40))
		if rowLuid == luid && rowIndex == index {
			var delRow mibUnicastIpAddressRow
			copy(delRow[:], (*[80]byte)(unsafe.Pointer(rowPtr))[:])
			procDeleteUnicastIpAddressEntry.Call(uintptr(unsafe.Pointer(&delRow[0])))
		}
	}
	return nil
}

// deleteNeighborAPI deletes a neighbor/ARP entry.
func deleteNeighborAPI(ip net.IP, index uint32, luid uint64) error {
	var row mibIpNetRow2
	row.setAddress(ip)
	row.setInterfaceIndex(index)
	row.setInterfaceLuid(luid)
	ret, _, _ := procDeleteIpNetEntry2.Call(uintptr(unsafe.Pointer(&row[0])))
	if ret != 0 && ret != 0xa5 { // 0xa5 = ERROR_NOT_FOUND
		return fmt.Errorf("DeleteIpNetEntry2: 0x%x", ret)
	}
	return nil
}

// flushNeighborsByIPAPI deletes all neighbor entries for the given IP across all interfaces.
func flushNeighborsByIPAPI(ip net.IP) error {
	var table uintptr
	ret, _, _ := procGetIpNetTable2.Call(
		uintptr(windows.AF_INET),
		uintptr(unsafe.Pointer(&table)),
	)
	if ret != 0 {
		return fmt.Errorf("GetIpNetTable2: 0x%x", ret)
	}
	defer procFreeMibTable.Call(table)

	if table == 0 {
		return nil
	}

	count := *(*uint32)(unsafe.Pointer(table))
	// Sanity check: if count is unreasonably large, the table is likely corrupt
	if count > 10000 {
		return fmt.Errorf("GetIpNetTable2: invalid count %d", count)
	}
	// MIB_IPNET_ROW2 has uint64 alignment (InterfaceLuid), so the Table
	// array starts at offset 8 (4 bytes NumEntries + 4 bytes padding).
	base := table + 8
	rowSize := uintptr(80) // MIB_IPNET_ROW2 size on Windows 10+

	for i := uint32(0); i < count; i++ {
		rowPtr := base + uintptr(i)*rowSize
		family := *(*uint16)(unsafe.Pointer(rowPtr))
		if family != windows.AF_INET {
			continue
		}
		rowIP := net.IP((*[4]byte)(unsafe.Pointer(rowPtr + 4))[:])
		if !rowIP.Equal(ip) {
			continue
		}
		rowIndex := *(*uint32)(unsafe.Pointer(rowPtr + 28))
		rowLuid := *(*uint64)(unsafe.Pointer(rowPtr + 32))
		_ = deleteNeighborAPI(ip, rowIndex, rowLuid)
	}
	return nil
}

// getDefaultGatewayAPI returns the default gateway IP, LUID, and interface index.
func getDefaultGatewayAPI() (net.IP, uint64, uint32, error) {
	var table uintptr
	ret, _, _ := procGetIpForwardTable2.Call(
		uintptr(windows.AF_INET),
		uintptr(unsafe.Pointer(&table)),
	)
	if ret != 0 {
		return nil, 0, 0, fmt.Errorf("GetIpForwardTable2: 0x%x", ret)
	}
	defer procFreeMibTable.Call(table)

	if table == 0 {
		return nil, 0, 0, fmt.Errorf("empty route table")
	}

	count := *(*uint32)(unsafe.Pointer(table))
	// MIB_IPFORWARD_ROW2 starts with NET_LUID (8-byte aligned), so the Table
	// array begins at offset 8 (4 bytes NumEntries + 4 bytes padding).
	base := table + 8
	rowSize := uintptr(104)

	var bestGW net.IP
	var bestLuid uint64
	var bestIdx uint32
	bestMetric := ^uint32(0)

	for i := uint32(0); i < count; i++ {
		rowPtr := base + uintptr(i)*rowSize
		var row mibIpForwardRow2
		copy(row[:], (*[104]byte)(unsafe.Pointer(rowPtr))[:])

		if !row.isDefaultRoute() {
			continue
		}
		gw := row.nextHop()
		if gw == nil || gw.IsUnspecified() {
			continue
		}

		luid := *(*uint64)(unsafe.Pointer(rowPtr))
		idx := *(*uint32)(unsafe.Pointer(rowPtr + 8))
		metric := *(*uint32)(unsafe.Pointer(rowPtr + 84))

		if metric < bestMetric {
			bestMetric = metric
			bestGW = gw
			bestLuid = luid
			bestIdx = idx
		}
	}

	if bestGW == nil {
		return nil, 0, 0, fmt.Errorf("no default gateway found")
	}
	return bestGW, bestLuid, bestIdx, nil
}

// DNS_SETTING_NAMESERVER flag indicates we're configuring the NameServer field
const DNS_SETTING_NAMESERVER = 0x0002

// interfaceDnsSettingsEx represents the correct DNS_INTERFACE_SETTINGS structure
// See: https://learn.microsoft.com/en-us/windows/win32/api/netioapi/ns-netioapi-dns_interface_settings
type interfaceDnsSettingsEx struct {
	Version             uint32
	_                   uint32 // padding to align Flags to 64-bit
	Flags               uint64 // ULONG64, not ULONG!
	Domain              *uint16
	NameServer          *uint16
	SearchList          *uint16
	RegistrationEnabled uint32
	RegisterAdapterName uint32
	EnableLLMNR         uint32
	QueryAdapterName    uint32
	ProfileNameServer   *uint16
}

// setInterfaceDNSAPI sets DNS servers for the interface using SetInterfaceDnsSettings.
func setInterfaceDNSAPI(luid uint64, index uint32, servers []net.IP) error {
	row, err := getIfEntry2API(luid)
	if err != nil {
		return err
	}

	// Build space-separated DNS server string (API accepts space or comma separated)
	serverStr := ""
	for i, s := range servers {
		if i > 0 {
			serverStr += " "
		}
		serverStr += s.String()
	}

	serverUTF16, err := windows.UTF16PtrFromString(serverStr)
	if err != nil {
		return err
	}

	// Use correct structure with Flags set
	settings := interfaceDnsSettingsEx{
		Version:    1,
		Flags:      DNS_SETTING_NAMESERVER, // Must set this flag!
		NameServer: serverUTF16,
	}

	ret, _, _ := procSetInterfaceDnsSettings.Call(
		uintptr(unsafe.Pointer(&row.InterfaceGuid)),
		uintptr(unsafe.Pointer(&settings)),
	)
	if ret != 0 {
		return fmt.Errorf("SetInterfaceDnsSettings: 0x%x", ret)
	}
	return nil
}

// clearInterfaceDNSAPI clears DNS servers for the interface (sets to empty/DHCP).
func clearInterfaceDNSAPI(luid uint64, index uint32) error {
	return setInterfaceDNSAPI(luid, index, nil)
}

// flushDNSResolverCacheAPI clears the Windows DNS resolver cache using DnsFlushResolverCache.
// This ensures that DNS entries cached before TUN started (real IPs) are discarded,
// so fresh queries go through the TUN DNS hijacker and return Fake-IPs.
func flushDNSResolverCacheAPI() error {
	ret, _, _ := procDnsFlushResolverCache.Call()
	if ret == 0 {
		return fmt.Errorf("DnsFlushResolverCache failed")
	}
	return nil
}

// getIfEntry2API calls GetIfEntry2 and returns the populated row.
func getIfEntry2API(luid uint64) (*windows.MibIfRow2, error) {
	var row windows.MibIfRow2
	row.InterfaceLuid = luid
	ret, _, _ := procGetIfEntry2.Call(uintptr(unsafe.Pointer(&row)))
	if ret != 0 {
		return nil, fmt.Errorf("GetIfEntry2: 0x%x", ret)
	}
	return &row, nil
}

// disableInterfaceAPI disables the named network interface using SetIfEntry.
func disableInterfaceAPI(name string) error {
	luid, _, err := getInterfaceLUID(name)
	if err != nil {
		return err
	}
	row, err := getIfEntry2API(luid)
	if err != nil {
		return err
	}
	row.AdminStatus = 2
	ret, _, _ := procSetIfEntry.Call(uintptr(unsafe.Pointer(row)))
	if ret != 0 {
		return fmt.Errorf("SetIfEntry: 0x%x", ret)
	}
	return nil
}

// removeAdapterAPI removes the named network adapter using SetupDi DIF_REMOVE.
// It matches Wintun adapters by checking if the device instance ID contains "WINTUN".
func removeAdapterAPI(name string) error {
	_, _, err := getInterfaceLUID(name)
	if err != nil {
		return err
	}

	devInfoSet, _, _ := procSetupDiGetClassDevsW.Call(
		uintptr(unsafe.Pointer(&guidDevClassNet)),
		0,
		0,
		0x02|0x04,
	)
	if devInfoSet == ^uintptr(0) {
		return fmt.Errorf("SetupDiGetClassDevs failed")
	}
	defer procSetupDiDestroyDeviceInfoList.Call(devInfoSet)

	for i := uint32(0); ; i++ {
		var devInfo spDevInfoData
		devInfo.init()
		ret, _, _ := procSetupDiEnumDeviceInfo.Call(devInfoSet, uintptr(i), uintptr(unsafe.Pointer(&devInfo[0])))
		if ret == 0 {
			break
		}

		var buf [256]uint16
		var reqSize uint32
		ret, _, _ = procSetupDiGetDeviceInstanceIdW.Call(
			devInfoSet,
			uintptr(unsafe.Pointer(&devInfo[0])),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
			uintptr(unsafe.Pointer(&reqSize)),
		)
		if ret == 0 {
			continue
		}
		instanceID := windows.UTF16ToString(buf[:])
		if !strings.Contains(strings.ToUpper(instanceID), "WINTUN") {
			continue
		}

		const DIF_REMOVE = 5
		ret, _, _ = procSetupDiCallClassInstaller.Call(
			uintptr(DIF_REMOVE),
			devInfoSet,
			uintptr(unsafe.Pointer(&devInfo[0])),
		)
		if ret != 0 {
			return fmt.Errorf("SetupDiCallClassInstaller DIF_REMOVE: error %d", ret)
		}
		return nil
	}

	return fmt.Errorf("adapter %s not found in device list", name)
}

// deleteResidualRoutesAPI scans the route table and deletes routes matching
// the given destination prefixes and gateway candidates.
func deleteResidualRoutesAPI() {
	targetDests := []struct {
		ip        net.IP
		prefixLen uint8
	}{
		{net.IPv4zero, 0},
		{net.ParseIP("198.18.0.0").To4(), 15},
	}
	targetGateways := []net.IP{
		net.IPv4zero,
		net.ParseIP("192.0.2.1").To4(),
		net.ParseIP("192.0.2.2").To4(),
		net.ParseIP("192.0.2.3").To4(),
		net.ParseIP("198.18.0.1").To4(),
		net.ParseIP("198.18.0.2").To4(),
	}

	var table uintptr
	ret, _, _ := procGetIpForwardTable2.Call(
		uintptr(windows.AF_INET),
		uintptr(unsafe.Pointer(&table)),
	)
	if ret != 0 {
		return
	}
	defer procFreeMibTable.Call(table)

	if table == 0 {
		return
	}

	count := *(*uint32)(unsafe.Pointer(table))
	// MIB_IPFORWARD_ROW2 starts with NET_LUID (8-byte aligned), so the Table
	// array begins at offset 8 (4 bytes NumEntries + 4 bytes padding).
	base := table + 8
	rowSize := uintptr(104)

	for i := uint32(0); i < count; i++ {
		rowPtr := base + uintptr(i)*rowSize
		var row mibIpForwardRow2
		copy(row[:], (*[104]byte)(unsafe.Pointer(rowPtr))[:])

		family := *(*uint16)(unsafe.Pointer(rowPtr + 12))
		if family != windows.AF_INET {
			continue
		}
		prefixLen := row[40]
		dstIP := net.IP((*[4]byte)(unsafe.Pointer(rowPtr + 16))[:])

		destMatch := false
		for _, td := range targetDests {
			if prefixLen == td.prefixLen && dstIP.Equal(td.ip) {
				destMatch = true
				break
			}
		}
		if !destMatch {
			continue
		}

		gw := row.nextHop()
		gwMatch := false
		for _, tg := range targetGateways {
			if gw.Equal(tg) {
				gwMatch = true
				break
			}
		}
		if !gwMatch {
			continue
		}

		procDeleteIpForwardEntry2.Call(uintptr(unsafe.Pointer(rowPtr)))
	}
}
