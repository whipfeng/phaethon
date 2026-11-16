//go:build !linux

package frame

import "net"

// carrierRetransStats is unavailable outside Linux (no portable TCP_INFO
// segment counters); the topology shows ctrl loss only on those nodes.
func carrierRetransStats(net.Conn) (uint64, uint64, bool) { return 0, 0, false }
