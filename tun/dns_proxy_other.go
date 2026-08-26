//go:build !windows

package tun

// DNSProxy is a no-op on non-Windows platforms; DNS queries reach the netstack
// hijacker directly through the TUN device.
type DNSProxy struct{}

// NewDNSProxy returns a no-op proxy.
func NewDNSProxy(e *Engine) *DNSProxy { return &DNSProxy{} }

// Start is a no-op.
func (p *DNSProxy) Start() error { return nil }

// Stop is a no-op.
func (p *DNSProxy) Stop() {}
