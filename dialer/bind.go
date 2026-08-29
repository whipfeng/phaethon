package dialer

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// BindContext holds the network context captured at TUN startup. It is used by
// DialRouteAware / ListenPacketRouteAware to bind outbound sockets to the
// correct physical interface, preventing traffic from looping back into the TUN
// device.
type BindContext struct {
	DefaultIfaceName   string
	DefaultIfaceIndex  int
	TUNLUID            uint64 // Windows only
	TUNIfaceName       string // Linux/Darwin: used to exclude TUN from route lookups
	DefaultGateway     net.IP
	OriginalDNSServers []string // upstream DNS before TUN redirect
}

var globalBindContext atomic.Pointer[BindContext]

// SetGlobalBindContext injects the context captured by the TUN engine. Passing
// nil clears the context and restores standard dial behavior.
func SetGlobalBindContext(bc *BindContext) {
	globalBindContext.Store(bc)
}

// GetGlobalBindContext returns the currently active BindContext, or nil if TUN
// is not enabled.
func GetGlobalBindContext() *BindContext {
	return globalBindContext.Load()
}

// BindSocket binds the raw socket to the interface that should carry traffic to
// dst. When dst is nil, it binds to the default physical interface. The actual
// platform logic lives in bind_*.go.
func (b *BindContext) BindSocket(c syscall.RawConn, dst net.IP) error {
	if b == nil {
		return nil
	}
	return b.bindSocket(c, dst)
}

// DialRouteAware dials addr using a socket bound to the correct physical
// interface. If no BindContext is active, it falls back to net.DialTimeout.
func DialRouteAware(network, addr string) (net.Conn, error) {
	bc := GetGlobalBindContext()
	if bc == nil {
		return net.DialTimeout(network, addr, 30*time.Second)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("route-aware: parse addr: %w", err)
	}

	// Already an IP: bind directly to the route-table chosen interface.
	if ip := net.ParseIP(host); ip != nil {
		return dialBound(network, addr, ip)
	}

	// Domain name: resolve through the upstream DNS (bypassing TUN DNS
	// hijacker) and then try each resolved IP.
	ips, err := ResolveRouteAware(host)
	if err != nil {
		return nil, fmt.Errorf("route-aware: resolve %s: %w", host, err)
	}

	var firstErr error
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		conn, err := dialBound(network, net.JoinHostPort(ip.String(), port), ip)
		if err == nil {
			return conn, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("no usable IP for %s", host)
	}
	return nil, firstErr
}

// ResolveRouteAware resolves host using the original upstream DNS servers with
// sockets bound to the interface determined by routing (excluding TUN interface).
// If no BindContext is active, it falls back to net.LookupHost.
func ResolveRouteAware(host string) ([]string, error) {
	bc := GetGlobalBindContext()
	if bc == nil {
		return net.LookupHost(host)
	}

	servers := bc.OriginalDNSServers
	if len(servers) == 0 {
		// No captured servers; fall back to standard resolution. This may be
		// hijacked by TUN DNS, but it is the best we can do.
		return net.LookupHost(host)
	}

	serverIP := net.ParseIP(servers[0])
	serverAddr := net.JoinHostPort(servers[0], "53")

	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			if bc != nil {
				d.Control = func(network, address string, c syscall.RawConn) error {
					return bc.BindSocket(c, serverIP)
				}
			}
			return d.DialContext(ctx, network, serverAddr)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return r.LookupHost(ctx, host)
}

// dialBound dials addr with a socket bound to the interface chosen for dst.
func dialBound(network, addr string, dst net.IP) (net.Conn, error) {
	bc := GetGlobalBindContext()
	d := net.Dialer{Timeout: 30 * time.Second}

	if bc != nil {
		d.Control = func(network, address string, c syscall.RawConn) error {
			return bc.BindSocket(c, dst)
		}
	}

	return d.Dial(network, addr)
}

// ListenPacketRouteAware creates a UDP socket bound to the default physical
// interface. If no BindContext is active, it falls back to ListenUDPWithAddr
// (respecting the global UDP port range).
func ListenPacketRouteAware(network, laddr string) (net.PacketConn, error) {
	bc := GetGlobalBindContext()
	if bc == nil {
		return ListenUDPWithAddr(laddr)
	}
	return listenPacketBound(network, laddr, nil, bc)
}

// ListenPacketBoundTo creates a UDP socket bound to the interface chosen for
// dst. If dst is nil, it binds to the default physical interface.
func ListenPacketBoundTo(network, laddr string, dst net.IP) (net.PacketConn, error) {
	bc := GetGlobalBindContext()
	if bc == nil {
		return ListenUDPWithAddr(laddr)
	}
	return listenPacketBound(network, laddr, dst, bc)
}

// listenPacketBound creates a PacketConn with an optional route-aware bind.
func listenPacketBound(network, laddr string, dst net.IP, bc *BindContext) (net.PacketConn, error) {
	if network == "" {
		network = "udp"
	}

	lc := net.ListenConfig{}

	if bc != nil {
		lc.Control = func(network, address string, c syscall.RawConn) error {
			return bc.BindSocket(c, dst)
		}
	}

	// Respect the global UDP port range if configured.
	if globalUDPPortMin > 0 && globalUDPPortMax >= globalUDPPortMin {
		return listenPacketWithPortRange(lc, network, laddr, dst, bc)
	}

	addr := laddr
	if addr == "" {
		addr = ":0"
	}
	return lc.ListenPacket(context.Background(), network, addr)
}

// listenPacketWithPortRange mirrors ListenUDPWithAddr logic but with a custom
// ListenConfig for route-aware binding.
func listenPacketWithPortRange(lc net.ListenConfig, network, laddr string, dst net.IP, bc *BindContext) (net.PacketConn, error) {
	ip := net.ParseIP(laddr)
	if ip == nil {
		ip = net.IPv4zero
	}

	start := globalUDPPortMin
	if lastUDPPort >= globalUDPPortMin && lastUDPPort < globalUDPPortMax {
		start = lastUDPPort + 1
	}
	for offset := 0; offset <= globalUDPPortMax-globalUDPPortMin; offset++ {
		port := start + offset
		if port > globalUDPPortMax {
			port = globalUDPPortMin + (port - globalUDPPortMax - 1)
		}
		addr := &net.UDPAddr{IP: ip, Port: port}
		conn, err := lc.ListenPacket(context.Background(), network, addr.String())
		if err == nil {
			lastUDPPort = port
			return conn, nil
		}
	}
	return nil, fmt.Errorf("no available UDP port in range %d-%d", globalUDPPortMin, globalUDPPortMax)
}

// routeCache caches (destination IP, network) -> chosen interface index/name.
// The value is a string; on Windows/Darwin it stores the interface index, on
// Linux the interface name.
type routeCacheEntry struct {
	value   string
	expires time.Time
}

var (
	routeCacheMu sync.RWMutex
	routeCache   = make(map[string]routeCacheEntry)
)

func routeCacheKey(dst net.IP, network string) string {
	return dst.String() + "#" + network
}

func cachedRoute(dst net.IP, network string) (string, bool) {
	if dst == nil {
		return "", false
	}
	routeCacheMu.RLock()
	defer routeCacheMu.RUnlock()
	ent, ok := routeCache[routeCacheKey(dst, network)]
	if !ok || time.Now().After(ent.expires) {
		return "", false
	}
	return ent.value, true
}

func setCachedRoute(dst net.IP, network string, value string) {
	if dst == nil {
		return
	}
	routeCacheMu.Lock()
	defer routeCacheMu.Unlock()
	routeCache[routeCacheKey(dst, network)] = routeCacheEntry{value: value, expires: time.Now().Add(30 * time.Second)}
}

func currentDefaultIndex(bc *BindContext) int {
	idx := bc.DefaultIfaceIndex
	if bc.DefaultIfaceName != "" {
		if iface, err := net.InterfaceByName(bc.DefaultIfaceName); err == nil {
			idx = iface.Index
		}
	}
	return idx
}

// indexFromCache parses a cached route value as an interface index.
func indexFromCache(s string) int {
	idx, _ := strconv.Atoi(s)
	return idx
}
