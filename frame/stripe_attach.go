package frame

import (
	"encoding/binary"
	"fmt"
)

// StripeAttach payload wire format:
//
//	{nodeIDLen:1}{nodeID:nodeIDLen}{seq:2}{stripeID:4}
//
// Sent as the first frame on a stripe connection (see
// docs/plans/mesh-multilink-stripe-design.md §5.2). The initiator declares
// its mesh node ID and local session seq so the passive side can bind the
// connection to the exact existing P2P session — (nodeID, seq) is the session
// incarnation key learned from gossip (peer.RemoteSeq) and is precise even
// when multiple sessions to the same node exist (one per proxy).
// stripeID is an opaque nonce chosen by the initiator to distinguish its
// multiple lanes within one session; it has no wire ordering semantics.

const maxStripeAttachNodeIDLen = 255

// EncodeStripeAttach builds the StripeAttach payload.
func EncodeStripeAttach(nodeID string, seq uint16, stripeID uint32) ([]byte, error) {
	if len(nodeID) == 0 || len(nodeID) > maxStripeAttachNodeIDLen {
		return nil, fmt.Errorf("stripe_attach: nodeID length %d out of range [1,%d]", len(nodeID), maxStripeAttachNodeIDLen)
	}
	payload := make([]byte, 1+len(nodeID)+2+4)
	payload[0] = byte(len(nodeID))
	copy(payload[1:], nodeID)
	binary.BigEndian.PutUint16(payload[1+len(nodeID):], seq)
	binary.BigEndian.PutUint32(payload[1+len(nodeID)+2:], stripeID)
	return payload, nil
}

// DecodeStripeAttach parses a StripeAttach payload.
func DecodeStripeAttach(payload []byte) (nodeID string, seq uint16, stripeID uint32, err error) {
	if len(payload) < 1+2+4 {
		return "", 0, 0, fmt.Errorf("stripe_attach: payload too short (%d)", len(payload))
	}
	idLen := int(payload[0])
	if idLen == 0 || 1+idLen+2+4 != len(payload) {
		return "", 0, 0, fmt.Errorf("stripe_attach: malformed payload (idLen=%d total=%d)", idLen, len(payload))
	}
	nodeID = string(payload[1 : 1+idLen])
	seq = binary.BigEndian.Uint16(payload[1+idLen:])
	stripeID = binary.BigEndian.Uint32(payload[1+idLen+2:])
	return nodeID, seq, stripeID, nil
}
