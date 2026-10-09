package frame

import (
	"bytes"
	"fmt"
	"net"
	"sync"
	"testing"
)

func TestStreamTransportConcurrentSendPreservesFrames(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	sender := NewStreamTransport(client)
	receiver := NewStreamTransport(server)
	defer sender.Close()
	defer receiver.Close()

	const framesPerType = 8
	var writers sync.WaitGroup
	for _, frameType := range []byte{FrameGossip, FrameMeshPacket} {
		frameType := frameType
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := range framesPerType {
				payload := []byte(fmt.Sprintf("%02x-%02d", frameType, i))
				if err := sender.Send(frameType, payload, frameType != FrameMeshPacket); err != nil {
					t.Errorf("Send(type=0x%02x, index=%d): %v", frameType, i, err)
					return
				}
			}
		}()
	}

	expected := make(map[string]struct{}, framesPerType*2)
	for _, frameType := range []byte{FrameGossip, FrameMeshPacket} {
		for i := range framesPerType {
			expected[fmt.Sprintf("%02x-%02d", frameType, i)] = struct{}{}
		}
	}

	received := make(map[string]struct{}, len(expected))
	for range expected {
		frameType, payload, err := receiver.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		key := string(payload)
		if _, ok := expected[key]; !ok {
			t.Fatalf("unexpected frame type=0x%02x payload=%q", frameType, payload)
		}
		if _, duplicate := received[key]; duplicate {
			t.Fatalf("duplicate payload %q", payload)
		}
		if !bytes.HasPrefix(payload, []byte(fmt.Sprintf("%02x-", frameType))) {
			t.Fatalf("frame type/payload mismatch: type=0x%02x payload=%q", frameType, payload)
		}
		received[key] = struct{}{}
	}

	writers.Wait()
}
