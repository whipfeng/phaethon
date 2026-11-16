//go:build !linux

package tun

// EnsureGatewayRules is a no-op outside Linux: the bypass-gateway host
// FORWARD tuples are managed by the platform-specific route implementation.
func (r *RouteManager) EnsureGatewayRules() {}
