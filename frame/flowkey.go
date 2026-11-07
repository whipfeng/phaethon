package frame

// InnerPacket returns the inner IPv4 packet of a one-level IPIP (protocol 4)
// datagram so classifiers see the real flow instead of the shared outer
// header. Anything else — non-IPv4 or non-IPIP outer, fragmented outer
// (non-first fragments carry no inner header), truncated, or non-IPv4
// inner — returns pkt unchanged.
func InnerPacket(pkt []byte) []byte {
	if len(pkt) < 20 || pkt[0]>>4 != 4 || pkt[9] != 4 {
		return pkt
	}
	if pkt[6]&0x20 != 0 || (uint16(pkt[6]&0x1F)<<8|uint16(pkt[7])) != 0 {
		return pkt
	}
	ihl := int(pkt[0]&0x0F) * 4
	if ihl < 20 || len(pkt) < ihl+20 {
		return pkt
	}
	if pkt[ihl]>>4 != 4 {
		return pkt
	}
	return pkt[ihl:]
}

// FlowKey returns a 32-bit key identifying the inner flow of an IPv4
// FrameMeshPacket payload: 5-tuple (srcIP, dstIP, protocol, srcPort, dstPort)
// hashed with Jenkins one-at-a-time; IPIP is keyed by its inner packet.
// Non-IPv4 or truncated payloads share key 0. Used to pin a flow to one
// mesh inbound worker; collisions only couple unrelated flows onto one
// queue, never break per-flow ordering.
func FlowKey(payload []byte) uint32 {
	payload = InnerPacket(payload)
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
