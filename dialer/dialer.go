package dialer

import (
	"fmt"
	"net"
	"strings"
	"time"

	"phaethon/config"
	"phaethon/reverse"
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
}

// UDPDialer establishes a UDP packet relay through a proxy.
// The returned net.PacketConn handles protocol-specific addressing internally;
// WriteTo sends data to the specified destination through the proxy, and
// ReadFrom returns data plus the source address as reported by the proxy.
type UDPDialer interface {
	DialPacket() (net.PacketConn, error)
}

// BaseDialer holds fields and logic common to all proxy dialers.
type BaseDialer struct {
	Proxy     *config.Proxy
	CmdType   byte   // 0=auto, 0x01=CONNECT, 0x02=BIND, 0x03=UDP_ASSOCIATE
	ConnID    string // correlates inbound and outbound logs for the same connection
	dialDepth int    // current chain depth for recursion guard
}

func (d *BaseDialer) SetConnID(id string) { d.ConnID = id }
func (d *BaseDialer) setDepth(depth int)  { d.dialDepth = depth }

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

// ResolveCmd returns the command to use. If CmdType is explicitly set, use it;
// otherwise auto-detect from dstPort (0 -> BIND).
func (d *BaseDialer) ResolveCmd(dstPort int) byte {
	if d.CmdType != 0 {
		return d.CmdType
	}
	if dstPort == 0 {
		return 0x02 // BIND
	}
	return 0x01 // CONNECT
}

// IsBind reports whether the resolved command is BIND (reverse).
func (d *BaseDialer) IsBind(dstPort int) bool {
	return d.ResolveCmd(dstPort) == 0x02
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
	framedConn := reverse.NewReverseFramedConn(conn)
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

// NewDialer creates a Dialer from a Proxy config (follows the proxy chain)
func NewDialer(proxy *config.Proxy) Dialer {
	if proxy == nil {
		return &DirectDialer{}
	}
	switch strings.ToUpper(proxy.Type) {
	case config.ProxyDIRECT:
		return &DirectDialer{}
	case config.ProxySOCKS5:
		return &Socks5Dialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyTROJAN:
		return &TrojanDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyH_TUNNEL:
		return &HTunnelDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyREVERSE:
		return &ReverseDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyHYSTERIA2:
		return &Hysteria2Dialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyVLESS:
		return &VLESSDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxySSH:
		return &SSHDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case "SS":
		return &ShadowsocksDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	case config.ProxyHTTP:
		return &HTTPDialer{BaseDialer: BaseDialer{Proxy: proxy}}
	default:
		return &stubDialer{name: proxy.Type}
	}
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
		return &ReverseDialer{BaseDialer: BaseDialer{Proxy: proxy}}
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
	case config.ProxyREVERSE:
		return &ReverseDialer{BaseDialer: BaseDialer{Proxy: proxy}}
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
