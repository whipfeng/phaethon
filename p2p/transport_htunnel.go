package p2p

import (
	"context"
	"fmt"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/frame"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/core/transport"
	"github.com/multiformats/go-multiaddr"
)

// H_TunnelTransport implements libp2p.Transport using H_Tunnel proxy
type H_TunnelTransport struct {
	proxyConfig *config.Proxy
}

// NewH_TunnelTransport creates a new H_Tunnel transport
func NewH_TunnelTransport(proxy *config.Proxy) *H_TunnelTransport {
	return &H_TunnelTransport{
		proxyConfig: proxy,
	}
}

// Dial dials a peer through H_Tunnel proxy
func (t *H_TunnelTransport) Dial(ctx context.Context, addr multiaddr.Multiaddr, p peer.ID) (transport.CapableConn, error) {
	// Create dialer from proxy config
	d := dialer.NewDialer(t.proxyConfig)
	
	// Type assert to HTunnelDialer to access DialP2P
	htunnelDialer, ok := d.(*dialer.HTunnelDialer)
	if !ok {
		return nil, fmt.Errorf("H_Tunnel transport requires HTunnelDialer, got %T", d)
	}
	
	// Dial through H_Tunnel proxy to get frame transport
	frameTransport, err := htunnelDialer.DialP2P()
	if err != nil {
		return nil, fmt.Errorf("H_Tunnel dial failed: %w", err)
	}
	
	// Wrap frame transport into libp2p connection
	return newH_TunnelCapableConn(frameTransport, p)
}

// Listen is not supported by H_Tunnel transport (client-only)
func (t *H_TunnelTransport) Listen(addr multiaddr.Multiaddr) (transport.Listener, error) {
	return nil, fmt.Errorf("H_Tunnel transport does not support listening")
}

// CanDial returns true if this transport can dial the given address
func (t *H_TunnelTransport) CanDial(addr multiaddr.Multiaddr) bool {
	// H_Tunnel transport can dial any address (it's a proxy)
	return true
}

// GetProxy returns the proxy used by this transport
func (t *H_TunnelTransport) GetProxy() *config.Proxy {
	return t.proxyConfig
}

// Proxy returns true if this transport uses a proxy
func (t *H_TunnelTransport) Proxy() bool {
	return true
}

// LocalSeq returns the local sequence number for this transport
func (t *H_TunnelTransport) LocalSeq() uint32 {
	return 0
}

// Protocols returns the set of protocols handled by this transport
func (t *H_TunnelTransport) Protocols() []int {
	// H_Tunnel transport doesn't use multiaddr protocols
	return nil
}

// String returns the name of this transport
func (t *H_TunnelTransport) String() string {
	return "h_tunnel"
}

// hTunnelCapableConn wraps frame.Transport to implement libp2p transport.CapableConn
type hTunnelCapableConn struct {
	transport.CapableConn
	
	frameTransport frame.FrameTransport
	remotePeer     peer.ID
	
	localAddr      multiaddr.Multiaddr
	remoteAddr     multiaddr.Multiaddr
}

// newH_TunnelCapableConn creates a new H_Tunnel capable connection
func newH_TunnelCapableConn(ft frame.FrameTransport, remotePeer peer.ID) (transport.CapableConn, error) {
	// Create dummy addresses (H_Tunnel proxy hides real addresses)
	localAddr, _ := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	remoteAddr, _ := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	
	return &hTunnelCapableConn{
		frameTransport: ft,
		remotePeer:     remotePeer,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
	}, nil
}

// Close closes the connection
func (c *hTunnelCapableConn) Close() error {
	return c.frameTransport.Close()
}

// LocalPeer returns the local peer ID (not applicable for H_Tunnel)
func (c *hTunnelCapableConn) LocalPeer() peer.ID {
	return ""
}

// RemotePeer returns the remote peer ID
func (c *hTunnelCapableConn) RemotePeer() peer.ID {
	return c.remotePeer
}

// RemotePublicKey returns the remote peer's public key (not applicable for H_Tunnel)
func (c *hTunnelCapableConn) RemotePublicKey() crypto.PubKey {
	return nil
}

// LocalMultiaddr returns the local multiaddr
func (c *hTunnelCapableConn) LocalMultiaddr() multiaddr.Multiaddr {
	return c.localAddr
}

// RemoteMultiaddr returns the remote multiaddr
func (c *hTunnelCapableConn) RemoteMultiaddr() multiaddr.Multiaddr {
	return c.remoteAddr
}

// Security returns the security protocol used (not applicable for H_Tunnel)
func (c *hTunnelCapableConn) Security() protocol.ID {
	return ""
}

// Multiplexer returns the multiplexer used (not applicable for H_Tunnel)
func (c *hTunnelCapableConn) Multiplexer() protocol.ID {
	return ""
}

// LocalSequence returns the local sequence number
func (c *hTunnelCapableConn) LocalSequence() uint32 {
	return 0
}

// FrameTransport returns the underlying frame transport
func (c *hTunnelCapableConn) FrameTransport() frame.FrameTransport {
	return c.frameTransport
}

// ConnState returns the connection state
func (c *hTunnelCapableConn) ConnState() network.ConnectionState {
	return network.ConnectionState{}
}
