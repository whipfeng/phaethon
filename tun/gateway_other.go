//go:build !linux

package tun

// gatewayNAT is a no-op outside Linux: the host-LAN bypass gateway (kernel
// forwarding + iptables MASQUERADE) is implemented on Linux only. Windows
// uses ICS/Wintun semantics and Darwin has no equivalent iptables path.
type gatewayNAT struct{}

func setupGatewayNAT(lanIface, upstreamIface string) (*gatewayNAT, error) { return nil, nil }

func (g *gatewayNAT) Stop() {}
