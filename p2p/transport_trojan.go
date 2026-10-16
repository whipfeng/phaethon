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

// TrojanTransport implements libp2p.Transport using trojan proxy
type TrojanTransport struct {
	proxyConfig *config.Proxy
	localSeq    uint32
}

// NewTrojanTransport creates a new trojan transport
func NewTrojanTransport(proxy *config.Proxy, localSeq uint32) *TrojanTransport {
	return &TrojanTransport{
		proxyConfig: proxy,
		localSeq:    localSeq,
	}
}

// Dial dials a peer through trojan proxy
func (t *TrojanTransport) Dial(ctx context.Context, addr multiaddr.Multiaddr, p peer.ID) (transport.CapableConn, error) {
	// Create dialer from proxy config
	d := dialer.NewDialer(t.proxyConfig)
	
	// Type assert to TrojanDialer to access DialP2P
	trojanDialer, ok := d.(*dialer.TrojanDialer)
	if !ok {
		// If it's wrapped (e.g., meshAwareDialer), try to get the underlying dialer
		// For now, we'll use the Dialer interface's methods
		return nil, fmt.Errorf("trojan transport requires TrojanDialer, got %T", d)
	}
	
	// Dial through trojan proxy to get frame transport
	frameTransport, err := trojanDialer.DialP2P()
	if err != nil {
		return nil, fmt.Errorf("trojan dial failed: %w", err)
	}
	
	// Wrap frame transport into libp2p connection
	return newTrojanCapableConn(frameTransport, p, t.localSeq)
}

// Listen is not supported by trojan transport (client-only)
func (t *TrojanTransport) Listen(addr multiaddr.Multiaddr) (transport.Listener, error) {
	return nil, fmt.Errorf("trojan transport does not support listening")
}

// CanDial returns true if this transport can dial the given address
func (t *TrojanTransport) CanDial(addr multiaddr.Multiaddr) bool {
	// Trojan transport can dial any address (it's a proxy)
	return true
}

// GetProxy returns the proxy used by this transport
func (t *TrojanTransport) GetProxy() *config.Proxy {
	return t.proxyConfig
}

// Proxy returns true if this transport uses a proxy
func (t *TrojanTransport) Proxy() bool {
	return true
}

// LocalSeq returns the local sequence number for this transport
func (t *TrojanTransport) LocalSeq() uint32 {
	return t.localSeq
}

// Protocols returns the set of protocols handled by this transport
func (t *TrojanTransport) Protocols() []int {
	// Trojan transport doesn't use multiaddr protocols
	return nil
}

// String returns the name of this transport
func (t *TrojanTransport) String() string {
	return "trojan"
}

// trojanCapableConn wraps frame.Transport to implement libp2p transport.CapableConn
type trojanCapableConn struct {
	transport.CapableConn
	
	frameTransport frame.FrameTransport
	remotePeer     peer.ID
	localSeq       uint32
	localAddr      multiaddr.Multiaddr
	remoteAddr     multiaddr.Multiaddr
}

// newTrojanCapableConn creates a new trojan capable connection
func newTrojanCapableConn(ft frame.FrameTransport, remotePeer peer.ID, localSeq uint32) (transport.CapableConn, error) {
	// Create dummy addresses (trojan proxy hides real addresses)
	localAddr, _ := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	remoteAddr, _ := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	
	return &trojanCapableConn{
		frameTransport: ft,
		remotePeer:     remotePeer,
		localSeq:       localSeq,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
	}, nil
}

// Close closes the connection
func (c *trojanCapableConn) Close() error {
	return c.frameTransport.Close()
}

// LocalPeer returns the local peer ID (not applicable for trojan)
func (c *trojanCapableConn) LocalPeer() peer.ID {
	return ""
}

// RemotePeer returns the remote peer ID
func (c *trojanCapableConn) RemotePeer() peer.ID {
	return c.remotePeer
}

// RemotePublicKey returns the remote peer's public key (not applicable for trojan)
func (c *trojanCapableConn) RemotePublicKey() crypto.PubKey {
	return nil
}

// LocalMultiaddr returns the local multiaddr
func (c *trojanCapableConn) LocalMultiaddr() multiaddr.Multiaddr {
	return c.localAddr
}

// RemoteMultiaddr returns the remote multiaddr
func (c *trojanCapableConn) RemoteMultiaddr() multiaddr.Multiaddr {
	return c.remoteAddr
}

// Security returns the security protocol used (not applicable for trojan)
func (c *trojanCapableConn) Security() protocol.ID {
	return ""
}

// Multiplexer returns the multiplexer used (not applicable for trojan)
func (c *trojanCapableConn) Multiplexer() protocol.ID {
	return ""
}

// LocalSequence returns the local sequence number
func (c *trojanCapableConn) LocalSequence() uint32 {
	return c.localSeq
}

// FrameTransport returns the underlying frame transport
func (c *trojanCapableConn) FrameTransport() frame.FrameTransport {
	return c.frameTransport
}

// ConnState returns the connection state
func (c *trojanCapableConn) ConnState() network.ConnectionState {
	return network.ConnectionState{}
}
