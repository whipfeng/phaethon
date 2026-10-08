package dialer

import (
	"context"
	"fmt"
	"net"

	"phaethon/config"
	"phaethon/frame"
	"phaethon/reverse"
)

// htunnelBindStrategy implements BindStrategy for the h_tunnel protocol.
//
// h_tunnel is special: it has a true v2 path (MESH single-step HEAD + frame
// transport) but can be configured to fall back to the v1 BIND-stream channel
// when proxy.P2P is disabled. The dispatch is:
//
//	BindModeData    → v1 three-step HEAD with "BIND" cmd, PORT=0
//	BindModeControl → v1 three-step HEAD with "BIND" cmd, PORT=1
// h_tunnel BindModeP2P is currently forced to v1 BIND-stream (PORT=2 + frame
// transport) regardless of proxy.IsP2P(), per 2026-10-08 v2 revert. The v2
// MESH direct path is preserved in dialHTunnelMeshDirect and can be re-enabled
// by uncommenting the gated branch in the BindModeP2P case below.
//
// SupportsP2P returns true because h_tunnel has a real v2 (MESH) path.
type htunnelBindStrategy struct{}

func (s htunnelBindStrategy) Bind(ctx context.Context, p *config.Proxy, mode BindMode, dstAddr string) (*BindResult, error) {
	switch mode {
	case BindModeData:
		// Reverse data connection: v1 BIND three-step, PORT=0.
		conn, err := dialHTunnelBIND(p, dstAddr, reverse.BindPortData)
		if err != nil {
			return nil, err
		}
		return &BindResult{Conn: conn, Mode: mode}, nil

	case BindModeControl:
		// Control/listen channel: v1 BIND three-step, PORT=1, dst=proxy.Server.
		conn, err := dialHTunnelBIND(p, p.Server, reverse.BindPortControl)
		if err != nil {
			return nil, err
		}
		return &BindResult{Conn: conn, Mode: mode}, nil

	case BindModeP2P:
		// v2 MESH direct DISABLED (2026-10-08): always use v1 BIND-stream fallback.
		// The v1 path goes through the h_tunnel proxy server's BIND PORT=2 channel,
		// wrapped in frame.NewStreamTransport. This matches the original h_tunnel
		// protocol behavior. Re-enable MESH direct when v2 design is settled.
		//
		// if p.IsP2P() {
		// 	transport, err := dialHTunnelMeshDirect(p)
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// 	return &BindResult{Frame: transport, Mode: mode}, nil
		// }
		conn, err := dialHTunnelBIND(p, p.Server, reverse.BindPortP2P)
		if err != nil {
			return nil, err
		}
		return &BindResult{
			Conn:  conn,
			Frame: frame.NewStreamTransport(conn),
			Mode:  mode,
		}, nil

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
