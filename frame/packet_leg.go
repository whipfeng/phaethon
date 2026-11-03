package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// UDP leg datagram codec: one frame == one datagram. Wire format is
// byte-identical to the TCP stream framing ([type:1B][len:2B][payload]);
// there is no segmentation concept — oversize datagrams are left to outer
// IP fragmentation (see docs/plans/p2p_udp_transport_design.md §3).

// EncodeDatagram serializes one frame into a single UDP datagram.
func EncodeDatagram(frameType byte, payload []byte) []byte {
	return serializeFrame(frameType, payload)
}

// ErrDatagramMalformed marks a datagram that fails frame-level validation.
var ErrDatagramMalformed = errors.New("packet_leg: malformed datagram")

// ParseDatagramFrame decodes one datagram into (frameType, payload).
// The payload is a zero-copy view into datagram — callers must not mutate
// it beyond the datagram's lifetime. Malformed datagrams (truncated header,
// declared length mismatch) are rejected; a rejected datagram is simply
// dropped (datagram semantics permit loss).
func ParseDatagramFrame(datagram []byte) (byte, []byte, error) {
	if len(datagram) < FrameHeaderSize {
		return 0, nil, fmt.Errorf("%w: too short (%d bytes)", ErrDatagramMalformed, len(datagram))
	}
	frameType := datagram[0]
	length := int(binary.BigEndian.Uint16(datagram[1:]))
	if length > MaxPayload {
		return 0, nil, fmt.Errorf("%w: declared length %d exceeds max %d", ErrDatagramMalformed, length, MaxPayload)
	}
	if length != len(datagram)-FrameHeaderSize {
		return 0, nil, fmt.Errorf("%w: declared length %d != actual %d", ErrDatagramMalformed, length, len(datagram)-FrameHeaderSize)
	}
	return frameType, datagram[FrameHeaderSize:], nil
}

// IsLegFrame reports whether frameType may ride the P2P UDP leg.
// Gossip (0x12) stays on TCP permanently — it doubles as the negotiation
// channel — so it is deliberately not a leg frame.
func IsLegFrame(frameType byte) bool {
	return frameType == FrameLinkPing || frameType == FrameLinkPong || frameType == FrameMeshPacket
}
