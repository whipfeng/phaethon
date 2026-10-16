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

// SOCKS5Transport implements libp2p.Transport using SOCKS5 proxy
type SOCKS5Transport struct {
	proxyConfig *config.Proxy
}

// NewSOCKS5Transport creates a new SOCKS5 transport
func NewSOCKS5Transport(proxy *config.Proxy) *SOCKS5Transport {
	return &SOCKS5Transport{
		proxyConfig: proxy,
	}
}

// Dial dials a peer through SOCKS5 proxy
func (t *SOCKS5Transport) Dial(ctx context.Context, addr multiaddr.Multiaddr, p peer.ID) (transport.CapableConn, error) {
	// Create dialer from proxy config
	d := dialer.NewDialer(t.proxyConfig)
	
	// Type assert to Socks5Dialer to access DialP2P
	socks5Dialer, ok := d.(*dialer.Socks5Dialer)
	if !ok {
		return nil, fmt.Errorf("SOCKS5 transport requires Socks5Dialer, got %T", d)
	}
	
	// Dial through SOCKS5 proxy to get frame transport
	frameTransport, err := socks5Dialer.DialP2P()
	if err != nil {
		return nil, fmt.Errorf("SOCKS5 dial failed: %w", err)
	}
	
	// Wrap frame transport into libp2p connection
	return newSOCKS5CapableConn(frameTransport, p)
}

// Listen is not supported by SOCKS5 transport (client-only)
func (t *SOCKS5Transport) Listen(addr multiaddr.Multiaddr) (transport.Listener, error) {
	return nil, fmt.Errorf("SOCKS5 transport does not support listening")
}

// CanDial returns true if this transport can dial the given address
func (t *SOCKS5Transport) CanDial(addr multiaddr.Multiaddr) bool {
	// SOCKS5 transport can dial any address (it's a proxy)
	return true
}

// GetProxy returns the proxy used by this transport
func (t *SOCKS5Transport) GetProxy() *config.Proxy {
	return t.proxyConfig
}

// Proxy returns true if this transport uses a proxy
func (t *SOCKS5Transport) Proxy() bool {
	return true
}

// LocalSeq returns the local sequence number for this transport
func (t *SOCKS5Transport) LocalSeq() uint32 {
	return 0
}

// Protocols returns the set of protocols handled by this transport
func (t *SOCKS5Transport) Protocols() []int {
	// SOCKS5 transport doesn't use multiaddr protocols
	return nil
}

// String returns the name of this transport
func (t *SOCKS5Transport) String() string {
	return "socks5"
}

// socks5CapableConn wraps frame.Transport to implement libp2p transport.CapableConn
type socks5CapableConn struct {
	transport.CapableConn
	
	frameTransport frame.FrameTransport
	remotePeer     peer.ID
	
	localAddr      multiaddr.Multiaddr
	remoteAddr     multiaddr.Multiaddr
}

// newSOCKS5CapableConn creates a new SOCKS5 capable connection
func newSOCKS5CapableConn(ft frame.FrameTransport, remotePeer peer.ID) (transport.CapableConn, error) {
	// Create dummy addresses (SOCKS5 proxy hides real addresses)
	localAddr, _ := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	remoteAddr, _ := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	
	return &socks5CapableConn{
		frameTransport: ft,
		remotePeer:     remotePeer,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
	}, nil
}

// Close closes the connection
func (c *socks5CapableConn) Close() error {
	return c.frameTransport.Close()
}

// LocalPeer returns the local peer ID (not applicable for SOCKS5)
func (c *socks5CapableConn) LocalPeer() peer.ID {
	return ""
}

// RemotePeer returns the remote peer ID
func (c *socks5CapableConn) RemotePeer() peer.ID {
	return c.remotePeer
}

// RemotePublicKey returns the remote peer's public key (not applicable for SOCKS5)
func (c *socks5CapableConn) RemotePublicKey() crypto.PubKey {
	return nil
}

// LocalMultiaddr returns the local multiaddr
func (c *socks5CapableConn) LocalMultiaddr() multiaddr.Multiaddr {
	return c.localAddr
}

// RemoteMultiaddr returns the remote multiaddr
func (c *socks5CapableConn) RemoteMultiaddr() multiaddr.Multiaddr {
	return c.remoteAddr
}

// Security returns the security protocol used (not applicable for SOCKS5)
func (c *socks5CapableConn) Security() protocol.ID {
	return ""
}

// Multiplexer returns the multiplexer used (not applicable for SOCKS5)
func (c *socks5CapableConn) Multiplexer() protocol.ID {
	return ""
}

// LocalSequence returns the local sequence number
func (c *socks5CapableConn) LocalSequence() uint32 {
	return 0
}

// FrameTransport returns the underlying frame transport
func (c *socks5CapableConn) FrameTransport() frame.FrameTransport {
	return c.frameTransport
}

// ConnState returns the connection state
func (c *socks5CapableConn) ConnState() network.ConnectionState {
	return network.ConnectionState{}
}
