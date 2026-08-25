package tun

import (
	"net"
	"sync"
)

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
}

// RouteSnapshot captures the route manager's current applied state.
type RouteSnapshot struct {
	Applied           bool     `json:"applied"`
	TUNIP             string   `json:"tunIP"`
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
		DefaultIface:      r.DefaultIfaceName,
		DefaultIfaceIndex: r.DefaultIfaceIndex,
		OriginalGateway:   gw,
		Exclusions:        excludes,
		SplitTunnels:      []string{"0.0.0.0/1", "128.0.0.0/1"},
	}
}
