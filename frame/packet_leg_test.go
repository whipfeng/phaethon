package frame

import (
	"bytes"
	"errors"
	"testing"
)

func TestDatagramRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		frameType byte
		payload   []byte
	}{
		{"mesh small", FrameMeshPacket, []byte("hello mesh")},
		{"link ping empty", FrameLinkPing, nil},
		{"link pong", FrameLinkPong, []byte{1, 2, 3}},
		{"mesh max payload", FrameMeshPacket, bytes.Repeat([]byte{0xAB}, MaxPayload)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			datagram := EncodeDatagram(tc.frameType, tc.payload)
			if len(datagram) != FrameHeaderSize+len(tc.payload) {
				t.Fatalf("encoded length = %d, want %d", len(datagram), FrameHeaderSize+len(tc.payload))
			}
			ft, payload, err := ParseDatagramFrame(datagram)
			if err != nil {
				t.Fatalf("ParseDatagramFrame: %v", err)
			}
			if ft != tc.frameType {
				t.Fatalf("frameType = 0x%02x, want 0x%02x", ft, tc.frameType)
			}
			if !bytes.Equal(payload, tc.payload) {
				t.Fatalf("payload mismatch: got %d bytes, want %d", len(payload), len(tc.payload))
			}
		})
	}
}

func TestParseDatagramFrameRejectsMalformed(t *testing.T) {
	cases := []struct {
		name     string
		datagram []byte
	}{
		{"empty", nil},
		{"header truncated", []byte{FrameMeshPacket, 0x00}},
		{"declared longer than actual", []byte{FrameMeshPacket, 0x00, 0x05, 'a', 'b'}},
		{"declared shorter than actual", []byte{FrameMeshPacket, 0x00, 0x01, 'a', 'b', 'c'}},
		{"zero declared with payload", []byte{FrameLinkPing, 0x00, 0x00, 'x'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseDatagramFrame(tc.datagram)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, ErrDatagramMalformed) {
				t.Fatalf("error %v does not wrap ErrDatagramMalformed", err)
			}
		})
	}
}

func TestParseDatagramFrameZeroCopy(t *testing.T) {
	payload := []byte("zero-copy view")
	datagram := EncodeDatagram(FrameMeshPacket, payload)
	_, parsed, err := ParseDatagramFrame(datagram)
	if err != nil {
		t.Fatalf("ParseDatagramFrame: %v", err)
	}
	if &parsed[0] != &datagram[FrameHeaderSize] {
		t.Fatalf("payload is not a zero-copy view into the datagram")
	}
}

func TestIsLegFrame(t *testing.T) {
	legit := []byte{FrameLinkPing, FrameLinkPong, FrameMeshPacket}
	for _, ft := range legit {
		if !IsLegFrame(ft) {
			t.Fatalf("IsLegFrame(0x%02x) = false, want true", ft)
		}
	}
	// gossip 恒走 TCP，绝不许上 UDP 腿
	if IsLegFrame(FrameGossip) {
		t.Fatalf("IsLegFrame(FrameGossip) = true, gossip must stay on TCP")
	}
	// 非 P2P 帧类型同样不允许
	for _, ft := range []byte{FrameHeartbeat, FrameData, 0x00, 0x7F, 0xFF} {
		if IsLegFrame(ft) {
			t.Fatalf("IsLegFrame(0x%02x) = true, want false", ft)
		}
	}
}
