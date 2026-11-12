package dialer

import (
	"context"
	"fmt"
	"net"

	"phaethon/config"
	"phaethon/frame"
)

// htunnelBindStrategy implements BindStrategy for the h_tunnel protocol.
//
// All channels go through the per-proxy carrier (HTTP vs WebSocket), and each
// mode has exactly one entry:
//
//	BindModeData    → carrier.DialBIND(PORT=0), WS when available
//	BindModeControl → carrier.DialBIND(PORT=1), WS when available
//	BindModeP2P     → carrier.DialP2P(): WS persistent frames when available,
//	                  otherwise MESH single-step HEAD (v2) or v1 BIND-stream.
//
// WebSocket availability is decided once per proxy; the v2 flag no longer
// gates carrier choice, it only selects the non-WS P2P handshake.
//
// SupportsP2P returns true because h_tunnel has a real v2 (MESH) path.
type htunnelBindStrategy struct{}

func (s htunnelBindStrategy) Bind(ctx context.Context, p *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error) {
	switch mode {
	case BindModeData:
		// Reverse data connection: BIND PORT=0 over the selected carrier.
		conn, err := dialHTBINDWithFallback(ctx, p, BindModeData, dstAddr)
		if err != nil {
			return nil, err
		}
		return &BindResult{Conn: conn, Mode: mode}, nil

	case BindModeControl:
		// Control/listen channel: BIND PORT=1, dst=proxy.Server.
		conn, err := dialHTBINDWithFallback(ctx, p, BindModeControl, p.Server)
		if err != nil {
			return nil, err
		}
		return &BindResult{Conn: conn, Mode: mode}, nil

	case BindModeP2P:
		// One negotiation entry for both v1 and v2: the carrier decides.
		// WS available  → persistent WS frames (v2 flag does not gate it)
		// WS unavailable → v2 ? MESH single-step HEAD : v1 BIND-stream
		transport, err := dialHTP2PWithFallback(p)
		if err != nil {
			return nil, err
		}
		return &BindResult{Frame: transport, Mode: mode}, nil

	default:
		return nil, fmt.Errorf("htunnel: unsupported BindMode %d", mode)
	}
}

func (htunnelBindStrategy) SupportsP2P() bool { return true }

func init() {
	RegisterBindStrategy(config.ProxyH_TUNNEL, htunnelBindStrategy{})
}

// dialHTunnelBIND is the v1 three-step HEAD dial helper. Decoupled from
// HTunnelDialer so the BindStrategy can run without instantiating one.
// The receiver is only used to route through proxy.Next via NewDialer.
func dialHTunnelBIND(p *config.Proxy, dstAddr string, dstPort int) (net.Conn, error) {
	d := &HTunnelDialer{BaseDialer: BaseDialer{Proxy: p}}
	return d.dialHTunnel("BIND", dstAddr, dstPort)
}

// dialHTunnelMeshDirect is the v2 single-step MESH dial helper.
func dialHTunnelMeshDirect(p *config.Proxy) (frame.FrameTransport, error) {
	d := &HTunnelDialer{BaseDialer: BaseDialer{Proxy: p}}
	return d.dialP2PDirect()
}
