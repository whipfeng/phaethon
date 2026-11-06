package frame

// FlowKey returns a 32-bit key identifying the inner flow of an IPv4
// FrameMeshPacket payload: 5-tuple (srcIP, dstIP, protocol, srcPort, dstPort)
// hashed with Jenkins one-at-a-time. Non-IPv4 or truncated payloads share
// key 0. Used by ordered-delivery paths (h_tunnel per-flow serialization,
// mesh dispatch pinning); collisions only couple unrelated flows onto one
// ordered queue, never break ordering.
func FlowKey(payload []byte) uint32 {
	if len(payload) < 20 {
		return 0
	}
	if payload[0]>>4 != 4 {
		return 0
	}

	protocol := payload[9]
	ihl := int(payload[0]&0x0F) * 4
	if ihl < 20 {
		ihl = 20
	}

	var h uint32
	for i := 12; i < 20; i++ {
		h += uint32(payload[i])
		h += h << 10
		h ^= h >> 6
	}
	h += uint32(protocol)
	h += h << 10
	h ^= h >> 6

	if (protocol == 6 || protocol == 17) && len(payload) >= ihl+4 {
		for i := ihl; i < ihl+4; i++ {
			h += uint32(payload[i])
			h += h << 10
			h ^= h >> 6
		}
	}

	h += h << 3
	h ^= h >> 11
	h += h << 15
	return h
}
