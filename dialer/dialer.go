package dialer

import (
	"fmt"
	"net"
	"phaethon/config"
	"phaethon/frame"
	"phaethon/mesh"
	"phaethon/reverse"
	"phaethon/util"
	"strconv"
	"strings"
	"time"
)

// Global UDP ephemeral port range.
// Set by main() after config load. Default 0 means use OS-assigned ports.
var (
	globalUDPPortMin int = 0 // 0 = OS default
	globalUDPPortMax int = 0
	lastUDPPort      int = 0 // last allocated port within range, for round-robin
)

// SetUDPPortRange sets the global UDP ephemeral port range.
// Call from main() after loading config. Values of 0 mean use OS default.
func SetUDPPortRange(min, max int) {
	if min > 0 && max >= min {
		globalUDPPortMin = min
		globalUDPPortMax = max
	}
}

// ListenUDP creates a UDP listener with optional port range control.
// If a port range is configured via SetUDPPortRange, tries each port in sequence,
// skipping occupied ones. Falls back to OS default if range is not configured.
func ListenUDP() (net.PacketConn, error) {
	return ListenUDPWithAddr("0.0.0.0")
}

// ListenUDPWithAddr creates a UDP listener bound to the given network address
// with optional port range control.
func ListenUDPWithAddr(network string) (net.PacketConn, error) {
	if globalUDPPortMin == 0 || globalUDPPortMax == 0 {
		// No port range configured — try OS-assigned port with retries.
		// Windows may return WSAEACCES (10013) for :0 under load or
		// when ephemeral ports are temporarily exhausted.
		var err error
		for i := 0; i < 5; i++ {
			conn, err := net.ListenUDP("udp", nil)
			if err == nil {
				return conn, nil
			}
			time.Sleep(time.Duration(i+1) * time.Millisecond)
		}
		return nil, fmt.Errorf("listen udp :0 failed after 5 attempts: %w", err)
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
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(network), Port: port})
		if err == nil {
			lastUDPPort = port
			return conn, err
		}
	}
	return nil, fmt.Errorf("no available UDP port in range %d-%d", globalUDPPortMin, globalUDPPortMax)
}

// Dialer establishes a connection to the destination through a proxy chain
type Dialer interface {
	Dial(dstAddr string, dstPort int) (net.Conn, error)
	ServerAddr() (string, int)
}

// UDPDialer establishes a UDP packet relay through a proxy.
// The returned net.PacketConn handles protocol-specific addressing internally;
// WriteTo sends data to the specified destination through the proxy, and
// ReadFrom returns data plus the source address as reported by the proxy.
type UDPDialer interface {
	DialPacket() (net.PacketConn, error)
}

// ControlDialer establishes a control connection to the registry.
// Each proxy type that supports BIND implements this: the connection targets
// the proxy's own server (which IS the registry), using protocol-specific
// handshake with PORT=1 to mark it as a control channel.
type ControlDialer interface {
	DialControl() (net.Conn, error)
}

// P2PDialer establishes a P2P frame transport to a peer.
// Each proxy type that supports BIND implements this: the connection targets
// the proxy's own server using protocol-specific handshake with PORT=2.
type P2PDialer interface {
	DialP2P() (frame.FrameTransport, error)
}

// ReverseDialer establishes a reverse data connection.
// The connection targets the proxy's own server using BIND PORT=0,
// matching with a client's BIND on the proxy server.
// dstAddr is the reverse address (e.g. dynamic address) sent in the BIND
// request — the server validates it against known reverse/dynamic addresses.
type ReverseDialer interface {
	DialReverse(dstAddr string) (net.Conn, error)
}

// BaseDialer holds fields and logic common to all proxy dialers.
type BaseDialer struct {
	Proxy     *config.Proxy
	ConnID    string // correlates inbound and outbound logs for the same connection
	dialDepth int    // current chain depth for recursion guard
}

func (d *BaseDialer) SetConnID(id string) { d.ConnID = id }
func (d *BaseDialer) setDepth(depth int)  { d.dialDepth = depth }

// ServerAddr returns the effective host:port for connecting to this proxy server.
// Used for health checks and control connections. Subtypes may override to
// extract address from protocol-specific fields (e.g. h_tunnel URL).
func (d *BaseDialer) ServerAddr() (string, int) {
	return d.Proxy.Server, d.Proxy.Port
}

// ConnIDStr returns the connection ID for logging, or "N/A" if unset.
func (d *BaseDialer) ConnIDStr() string {
	if d.ConnID != "" {
		return d.ConnID
	}
	return "N/A"
}

type connIDSetter interface {
	SetConnID(string)
}

// TryReverse obtains a connection from the reverse registry if ReverseAddress is configured.
// The returned conn is already wrapped in ReverseFramedConn with a mode-indicator
// DATA frame sent, so callers can write application-layer bytes directly.
func (d *BaseDialer) TryReverse() (net.Conn, error) {
	if d.Proxy == nil || d.Proxy.ReverseAddress == "" {
		return nil, nil
	}
	registry := reverse.GlobalRegistry()
	if registry == nil {
		return nil, fmt.Errorf("reverse registry not initialized")
	}
	conn, err := registry.Match(d.Proxy.ReverseAddress)
	if err != nil {
		return nil, err
	}
	framedConn := frame.NewReverseFramedConn(conn)
	// Send empty DATA frame as mode indicator so the server-side handler
	// in StartReverseMapping can distinguish TCP vs UDP_CHANNEL mode.
	if _, werr := framedConn.Write(nil); werr != nil {
		framedConn.Close()
		return nil, fmt.Errorf("reverse: mode frame fail: %w", werr)
	}
	return framedConn, nil
}

// stubDialer returns an error for unsupported proxy types
type stubDialer struct {
	name string
}

func (s *stubDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	return nil, fmt.Errorf("proxy type '%s' is not yet implemented", s.name)
}

func (s *stubDialer) ServerAddr() (string, int) {
	return "", 0
}

// NewDialer creates a Dialer from a Proxy config (follows the proxy chain).
//
// When proxy.IsP2P() is true (socks5/trojan/h_tunnel with p2p enabled), the
// returned Dialer's Dial() bypasses protocol handshake and routes directly
// through the mesh IPIP tunnel (see docs/plans/dialer_bind_strategy.md §"v1/v2
// dispatch for Dial() 路径"). Other proxy types have IsP2P()==false and
// retain their v1 protocol handshake path unchanged.
func NewDialer(proxy *config.Proxy) Dialer {
	if proxy == nil {
		return &DirectDialer{}
	}
	var d Dialer
	switch strings.ToUpper(proxy.Type) {
	case config.ProxyDIRECT:
		return &DirectDialer{}
	case config.ProxySOCKS5:
		d = &Socks5Dialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyTROJAN:
		d = &TrojanDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyH_TUNNEL:
		d = &HTunnelDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyHYSTERIA2:
		d = &Hysteria2Dialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyVLESS:
		d = &VLESSDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxySSH:
		d = &SSHDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case "SS":
		d = &ShadowsocksDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyHTTP:
		d = &HTTPDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	default:
		return &stubDialer{name: proxy.Type}
	}

	// v2 dispatch: must have both p2p=true AND v2=true to use mesh direct path.
	// v2=false (default) keeps the v1 protocol-handshake path (stable baseline).
	if proxy.IsP2P() && proxy.V2 {
		return &meshAwareDialer{proxy: proxy, underlying: d}
	}
	return d
}

// meshAwareDialer replaces a protocol-handshake Dialer when proxy.IsP2P() is
// true. The data path does NOT traverse the proxy server (SOCKS5/Trojan/h_tunnel);
// instead, the local gVisor netstack handles the TCP connect and routes the IP
// packet through the mesh IPIP tunnel to the remote peer's netstack, which
// performs the actual outbound via its own physical interface.
//
// Dial() uses the v2 mesh path. Protocol-specific methods (DialControl/DialP2P/
// DialReverse/DialPacket) delegate to the underlying v1 dialer, because P2P
// session setup, control channels, reverse connections and UDP still use the
// proxy protocol handshake even in v2 mode.
type meshAwareDialer struct {
	proxy      *config.Proxy
	underlying Dialer
}

func (d *meshAwareDialer) Dial(dstAddr string, dstPort int) (net.Conn, error) {
	// v2 data path: create a per-endpoint RouteFunc that bypasses RouteSelector
	// and routes directly to the peer's LinkNIC with IPIP encapsulation.
	routeFunc := mesh.CreateV2RouteFunc(d.proxy.Name)

	if GlobalNetstackDialV2Func == nil {
		return nil, fmt.Errorf("v2 dial: netstack v2 dial not initialized")
	}

	var targetAddr string
	if ip := net.ParseIP(dstAddr); ip == nil {
		// Domain: resolve through netstack DNS to get Fake-IP
		if GlobalDNSResolverFunc == nil {
			return nil, fmt.Errorf("v2 dial: DNS resolver not available for domain: %s", dstAddr)
		}
		fakeIP, err := GlobalDNSResolverFunc(dstAddr)
		if err != nil {
			return nil, fmt.Errorf("v2 dial: resolve %s: %w", dstAddr, err)
		}
		targetAddr = net.JoinHostPort(fakeIP.String(), strconv.Itoa(dstPort))
	} else {
		targetAddr = net.JoinHostPort(dstAddr, strconv.Itoa(dstPort))
	}

	return GlobalNetstackDialV2Func("tcp", targetAddr, routeFunc)
}

// ServerAddr returns empty for mesh dialer: no proxy server is in the data path.
func (d *meshAwareDialer) ServerAddr() (string, int) { return "", 0 }

// DialP2P delegates to the underlying v1 dialer. P2P session setup still uses
// the proxy protocol handshake (PORT=2) even in v2 mode.
func (d *meshAwareDialer) DialP2P() (frame.FrameTransport, error) {
	if p, ok := d.underlying.(P2PDialer); ok {
		return p.DialP2P()
	}
	return nil, fmt.Errorf("proxy type '%s' does not support P2P", d.proxy.Type)
}

// DialControl delegates to the underlying v1 dialer. Control connections use
// the proxy protocol handshake (PORT=1).
func (d *meshAwareDialer) DialControl() (net.Conn, error) {
	if c, ok := d.underlying.(ControlDialer); ok {
		return c.DialControl()
	}
	return nil, fmt.Errorf("proxy type '%s' does not support control dial", d.proxy.Type)
}

// DialReverse delegates to the underlying v1 dialer. Reverse connections use
// the proxy protocol BIND (PORT=0).
func (d *meshAwareDialer) DialReverse(dstAddr string) (net.Conn, error) {
	if r, ok := d.underlying.(ReverseDialer); ok {
		return r.DialReverse(dstAddr)
	}
	return nil, fmt.Errorf("proxy type '%s' does not support reverse dial", d.proxy.Type)
}

// DialPacket delegates to the underlying v1 dialer. UDP forwarding still uses
// the proxy protocol's UDP ASSOCIATE.
func (d *meshAwareDialer) DialPacket() (net.PacketConn, error) {
	if u, ok := d.underlying.(UDPDialer); ok {
		return u.DialPacket()
	}
	return nil, fmt.Errorf("proxy type '%s' does not support UDP forwarding", d.proxy.Type)
}

const maxDialDepth = 10

// ChainDial dials through a proxy chain: first dials the bottom-most proxy,
// then each layer establishes its protocol on top
func ChainDial(proxy *config.Proxy, dstAddr string, dstPort int) (net.Conn, error) {
	return chainDial(proxy, dstAddr, dstPort, "", 0)
}

// ChainDialWithID is like ChainDial but attaches a connection ID so that
// outbound dial logs can be correlated with the inbound accept logs.
func ChainDialWithID(proxy *config.Proxy, dstAddr string, dstPort int, connID string) (net.Conn, error) {
	return chainDial(proxy, dstAddr, dstPort, connID, 0)
}

// DialToProxy dials through the proxy's via-chain to reach the proxy's own server.
// Used for TCP connectivity health checks.
func DialToProxy(p *config.Proxy) (net.Conn, error) {
	if p == nil {
		return nil, fmt.Errorf("nil proxy")
	}
	d := NewDialer(p)
	server, port := d.ServerAddr()
	util.LogModuleDebug("dialer", "%s: ServerAddr() = %s:%d", p.Name, server, port)
	if p.Next != nil && !strings.EqualFold(p.Next.Type, config.ProxyDIRECT) {
		util.LogModuleDebug("dialer", "%s: dialing via %s (%s:%d)", p.Name, p.Next.Name, server, port)
		nextDialer := NewDialer(p.Next)
		conn, err := nextDialer.Dial(server, port)
		if err != nil {
			util.LogModuleDebug("dialer", "%s: via dial failed: %v", p.Name, err)
			return nil, err
		}
		util.LogModuleDebug("dialer", "%s: via dial success", p.Name)
		return conn, nil
	}
	addr := net.JoinHostPort(server, strconv.Itoa(port))
	util.LogModuleDebug("dialer", "%s: direct dial to %s", p.Name, addr)
	conn, err := DialRouteAware("tcp", addr)
	if err != nil {
		util.LogModuleDebug("dialer", "%s: direct dial failed: %v", p.Name, err)
		return nil, err
	}
	util.LogModuleDebug("dialer", "%s: direct dial success", p.Name)
	return conn, nil
}

// PreWarmSSHProxies establishes SSH connections eagerly for all SSH-type proxies
// in the list so that the first user request does not pay the handshake latency.
func PreWarmSSHProxies(proxies []*config.Proxy) {
	for _, p := range proxies {
		if p == nil || !p.IsEnabled() {
			continue
		}
		if strings.EqualFold(p.Type, config.ProxySSH) {
			d := &SSHDialer{BaseDialer: BaseDialer{Proxy: p}}
			if err := d.PreWarm(); err != nil {
				util.LogModuleWarn("dialer", "pre-warm failed: %v", p.Name, err)
			}
		}
	}
}

func chainDial(proxy *config.Proxy, dstAddr string, dstPort int, connID string, depth int) (net.Conn, error) {
	if depth > maxDialDepth {
		return nil, fmt.Errorf("dial: max chain depth exceeded (%d), possible recursive proxy rule for %s:%d", maxDialDepth, dstAddr, dstPort)
	}

	d := NewDialer(proxy)
	if s, ok := d.(connIDSetter); ok && connID != "" {
		s.SetConnID(connID)
	}
	if dd, ok := d.(interface{ setDepth(int) }); ok {
		dd.setDepth(depth)
	}
	return d.Dial(dstAddr, dstPort)
}

// stubUDPDialer returns an error for unsupported proxy types
type stubUDPDialer struct {
	name string
}

func (s *stubUDPDialer) DialPacket() (net.PacketConn, error) {
	return nil, fmt.Errorf("proxy type '%s' does not support UDP forwarding", s.name)
}

// NewUDPDialer creates a UDPDialer from a Proxy config.
func NewUDPDialer(proxy *config.Proxy) UDPDialer {
	if proxy == nil {
		return &DirectDialer{}
	}
	if proxy.ReverseAddress != "" {
		return &reverseRegistryDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	}
	switch strings.ToUpper(proxy.Type) {
	case config.ProxyDIRECT:
		return &DirectDialer{}
	case config.ProxySOCKS5:
		return &Socks5Dialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case "SS":
		return &ShadowsocksDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyVLESS:
		return &VLESSDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyH_TUNNEL:
		return &HTunnelDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyTROJAN:
		return &TrojanDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyHYSTERIA2:
		return &Hysteria2Dialer{BaseDialer: BaseDialer{Proxy: proxy}}
	default:
		return &stubUDPDialer{name: proxy.Type}
	}
}

// ChainUDPDial creates a UDP tunnel through a proxy chain.
// For DIRECT or nil, uses a route-aware local UDP socket.
// For SOCKS5/Trojan/etc., creates their respective UDP ASSOCIATE,
// which tunnels TCP control through the next hop in the chain.
func ChainUDPDial(proxy *config.Proxy) (net.PacketConn, error) {
	if proxy == nil || strings.ToUpper(proxy.Type) == config.ProxyDIRECT {
		return (&DirectDialer{}).DialPacket()
	}
	d := NewUDPDialer(proxy)
	return d.DialPacket()
}
