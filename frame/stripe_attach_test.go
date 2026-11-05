package frame

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestStripeAttachRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		nodeID   string
		stripeID uint32
	}{
		{"short id", "vm", 1},
		{"longer id", "node-42", 0xDEADBEEF},
		{"max length id", strings.Repeat("n", maxStripeAttachNodeIDLen), 0},
		{"non-ascii id", "节点甲", 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := EncodeStripeAttach(tc.nodeID, tc.stripeID)
			if err != nil {
				t.Fatalf("EncodeStripeAttach: %v", err)
			}
			nodeID, stripeID, err := DecodeStripeAttach(payload)
			if err != nil {
				t.Fatalf("DecodeStripeAttach: %v", err)
			}
			if nodeID != tc.nodeID || stripeID != tc.stripeID {
				t.Fatalf("round trip mismatch: got (%q,%d) want (%q,%d)", nodeID, stripeID, tc.nodeID, tc.stripeID)
			}
		})
	}
}

func TestStripeAttachWireFormat(t *testing.T) {
	// Wire format is contractual (passive side parses it): {len:1}{id}{stripeID:4 BE}.
	payload, err := EncodeStripeAttach("qg", 0x01020304)
	if err != nil {
		t.Fatalf("EncodeStripeAttach: %v", err)
	}
	want := append([]byte{2}, []byte("qg")...)
	want = binary.BigEndian.AppendUint32(want, 0x01020304)
	if !bytes.Equal(payload, want) {
		t.Fatalf("wire mismatch: got %x want %x", payload, want)
	}
}

func TestStripeAttachInvalid(t *testing.T) {
	if _, err := EncodeStripeAttach("", 1); err == nil {
		t.Fatal("empty nodeID must fail")
	}
	if _, err := EncodeStripeAttach(strings.Repeat("n", maxStripeAttachNodeIDLen+1), 1); err == nil {
		t.Fatal("oversize nodeID must fail")
	}

	for _, payload := range [][]byte{
		nil,
		{0},
		{2, 'q'},                  // declared 2, only 1 present
		{0, 0, 0, 0, 0},           // zero idLen
		{3, 'q', 'g', 1, 2, 3, 4}, // trailing garbage
	} {
		if _, _, err := DecodeStripeAttach(payload); err == nil {
			t.Fatalf("payload %x must fail decode", payload)
		}
	}
}

func TestStripeAttachFrameRoundTrip(t *testing.T) {
	// The attach rides as a normal control frame through WriteFrame/ReadFrame.
	payload, err := EncodeStripeAttach("vm", 99)
	if err != nil {
		t.Fatalf("EncodeStripeAttach: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStripeAttach, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	ft, decoded, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	defer ReturnBuffer(decoded)
	if ft != FrameStripeAttach {
		t.Fatalf("frame type = 0x%02x, want 0x%02x", ft, FrameStripeAttach)
	}
	nodeID, stripeID, err := DecodeStripeAttach(decoded)
	if err != nil {
		t.Fatalf("DecodeStripeAttach: %v", err)
	}
	if nodeID != "vm" || stripeID != 99 {
		t.Fatalf("got (%q,%d)", nodeID, stripeID)
	}
}

func TestStripeAttachIsControlFrame(t *testing.T) {
	if !IsControlFrame(FrameStripeAttach) {
		t.Fatal("FrameStripeAttach must be classified as control frame")
	}
}
