package tun

import (
	"net"
	"sync"
)

// DefaultLANExclusions lists IPv4 private/local subnets that should bypass TUN
// to avoid breaking local network connectivity and multicast traffic.
var DefaultLANExclusions = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"224.0.0.0/4",
	"255.255.255.255/32",
}

// RouteManager handles OS route table modifications using platform-native APIs.
// Windows: iphlpapi.dll (CreateIpForwardEntry2, etc.)
// Linux:    netlink
// Darwin:   route exec (industry standard for macOS)
type RouteManager struct {
	devName         string
	guid            string
	mu              sync.Mutex
	applied         bool
	originalGateway net.IP
	excludeIPs      []string
	appliedExcludes []string
	tunIP           string

	// DefaultIfaceName is the original default interface name (e.g. "eth0", "en0").
	// Used for DIRECT connection socket binding to bypass TUN routes.
	DefaultIfaceName string
	// DefaultIfaceIndex is the original default interface index.
	// Used for socket options (IP_BOUND_IF on macOS, IP_UNICAST_IF on Windows).
	DefaultIfaceIndex int
	// defaultIfaceLUID is the original interface LUID (Windows only).
	// Used for exclusion route add/delete on the correct interface.
	defaultIfaceLUID uint64

	// tunLUID is the phaethon TUN interface LUID (Windows only).
	// Used to exclude the TUN interface from route lookups.
	tunLUID uint64
	// tunIndex is the phaethon TUN interface index. The watchdog uses this to
	// bind its probe sockets directly to the TUN interface.
	tunIndex int

	// OriginalDNSServers stores the DNS servers of the original default
	// interface before TUN redirects DNS, so TUN-internal resolution can use
	// the real upstream servers.
	OriginalDNSServers []string

	// originalPhysicalWeakHostReceive records whether weak-host receive was
	// enabled on the physical default interface before TUN setup (Windows only).
	// Used to restore the original setting during teardown.
	originalPhysicalWeakHostReceive bool
}

// RouteSnapshot captures the route manager's current applied state.
type RouteSnapshot struct {
	Applied           bool     `json:"applied"`
	TUNIP             string   `json:"tunIP"`
	TUNInterfaceIndex int      `json:"tunInterfaceIndex"`
	DefaultIface      string   `json:"defaultIface"`
	DefaultIfaceIndex int      `json:"defaultIfaceIndex"`
	OriginalGateway   string   `json:"originalGateway"`
	Exclusions        []string `json:"exclusions"`
	SplitTunnels      []string `json:"splitTunnels"`
}

// NewRouteManager creates a route manager for the given device.
func NewRouteManager(devName, guid string) *RouteManager {
	return &RouteManager{devName: devName, guid: guid}
}

// SetExclusions sets the list of proxy server IPs that should bypass TUN.
func (r *RouteManager) SetExclusions(ips []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.excludeIPs = ips
}

// TUNLUID returns the TUN interface LUID (Windows only).
func (r *RouteManager) TUNLUID() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tunLUID
}

// SetTUNLUID sets the TUN interface LUID when it is already known (e.g. from
// Wintun on Windows). A zero value means the platform setup should discover it.
func (r *RouteManager) SetTUNLUID(luid uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tunLUID = luid
}

// TUNInterfaceIndex returns the TUN interface index used by the watchdog to bind
// its probe sockets directly to the TUN adapter. It returns 0 if the index has
// not been set.
func (r *RouteManager) TUNInterfaceIndex() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tunIndex
}

// TUNInterfaceIP returns the IPv4 address assigned to the TUN adapter during
// Setup. It returns nil if Setup has not been called.
func (r *RouteManager) TUNInterfaceIP() net.IP {
	r.mu.Lock()
	defer r.mu.Unlock()
	ip := net.ParseIP(r.tunIP)
	if ip == nil {
		return nil
	}
	return ip.To4()
}

// OriginalGateway returns a copy of the original default gateway IP.
func (r *RouteManager) OriginalGateway() net.IP {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.originalGateway == nil {
		return nil
	}
	return append(net.IP(nil), r.originalGateway...)
}

// Setup configures routes to send all traffic through the TUN device.
func (r *RouteManager) Setup(tunIP string, prefixLen int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.applied {
		return nil
	}
	r.appliedExcludes = r.appliedExcludes[:0]

	r.tunIP = tunIP
	err := r.platformSetup(tunIP, prefixLen)
	if err != nil {
		r.tunIP = ""
		r.cleanupExclusions()
		return err
	}
	r.applied = true
	return nil
}

// Teardown removes the routes added by Setup.
func (r *RouteManager) Teardown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleanupExclusions()
	if !r.applied {
		return
	}
	r.platformTeardown()
	r.applied = false
}

// cleanupExclusions deletes only the host routes we successfully added.
func (r *RouteManager) cleanupExclusions() {
	for _, ip := range r.appliedExcludes {
		if ip == "" || r.originalGateway == nil {
			continue
		}
		r.deleteExclusionRoute(ip)
	}
	r.appliedExcludes = r.appliedExcludes[:0]
}

// Snapshot returns a copy of the current route state for UI display.
func (r *RouteManager) Snapshot() RouteSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()

	gw := ""
	if r.originalGateway != nil {
		gw = r.originalGateway.String()
	}
	excludes := make([]string, len(r.appliedExcludes))
	copy(excludes, r.appliedExcludes)

	return RouteSnapshot{
		Applied:           r.applied,
		TUNIP:             r.tunIP,
		TUNInterfaceIndex: r.tunIndex,
		DefaultIface:      r.DefaultIfaceName,
		DefaultIfaceIndex: r.DefaultIfaceIndex,
		OriginalGateway:   gw,
		Exclusions:        excludes,
		SplitTunnels:      []string{"0.0.0.0/1", "128.0.0.0/1"},
	}
}
