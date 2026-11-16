package frame

import "net"

// CarrierStatsProvider is implemented by transports that can report the
// carrier socket's cumulative TCP segment/retransmission counters.
type CarrierStatsProvider interface {
	CarrierRetransStats() (segs, retrans uint64, ok bool)
}

// CarrierRetransStats reports cumulative TCP segments sent and retransmitted
// for a carrier connection, when the OS exposes it (Linux TCP_INFO). Used by
// the mesh topology to show the real link retransmission rate, separate from
// the application-level ping loss rate (which TCP hides).
func CarrierRetransStats(conn net.Conn) (segs, retrans uint64, ok bool) {
	return carrierRetransStats(conn)
}

// TransportCarrierStats is the wrapper-friendly form for transports that hold
// a base FrameTransport.
func TransportCarrierStats(tr FrameTransport) (segs, retrans uint64, ok bool) {
	if p, ok := tr.(CarrierStatsProvider); ok {
		return p.CarrierRetransStats()
	}
	return 0, 0, false
}
