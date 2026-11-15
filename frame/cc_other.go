//go:build !linux

package frame

import "net"

// setCongestionControl is a no-op outside Linux: TCP_CONGESTION does not exist
// on Windows/macOS. The portable app-level pacer does not depend on it.
func setCongestionControl(*net.TCPConn, string) error { return nil }
