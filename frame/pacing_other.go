//go:build !linux

package frame

import "net"

// setMaxPacingRate is Linux-only (SO_MAX_PACING_RATE). Other platforms keep
// their existing carrier backpressure behavior.
func setMaxPacingRate(*net.TCPConn, uint64) error { return nil }
