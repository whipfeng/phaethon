package p2p

import (
	"testing"

	"phaethon/frame"
)

func TestFairLaneBatchesRoundRobin(t *testing.T) {
	var frames []frame.Frame
	for i := 0; i < 12; i++ {
		frames = append(frames,
			frame.Frame{Type: frame.FrameMeshPacket, Payload: []byte{'A', byte(i)}},
			frame.Frame{Type: frame.FrameMeshPacket, Payload: []byte{'B', byte(i)}},
		)
	}
	ka := flowHash32(1, []byte{'A', 0})
	kb := flowHash32(1, []byte{'B', 0})
	if ka == kb {
		t.Skip("test payloads hash to the same flow")
	}

	chunks := fairLaneBatches(1, frames, 4, 1<<20)
	if len(chunks) != 6 {
		t.Fatalf("chunks = %d, want 6", len(chunks))
	}
	next := map[byte]int{'A': 0, 'B': 0}
	for ci, chunk := range chunks {
		if len(chunk) > 4 {
			t.Fatalf("chunk %d has %d frames, want <=4", ci, len(chunk))
		}
		flow := chunk[0].Payload[0]
		for _, f := range chunk {
			if f.Payload[0] != flow {
				t.Fatalf("chunk %d mixes flows", ci)
			}
			if got := int(f.Payload[1]); got != next[flow] {
				t.Fatalf("flow %c out of order: got %d, want %d", flow, got, next[flow])
			}
			next[flow]++
		}
	}
	if next['A'] != 12 || next['B'] != 12 {
		t.Fatalf("flow counts A=%d B=%d, want 12/12", next['A'], next['B'])
	}
}

func TestFairLaneBatchesSingleFlowUnchanged(t *testing.T) {
	frames := []frame.Frame{
		{Type: frame.FrameMeshPacket, Payload: []byte("A0")},
		{Type: frame.FrameMeshPacket, Payload: []byte("A1")},
		{Type: frame.FrameMeshPacket, Payload: []byte("A2")},
	}
	chunks := fairLaneBatches(1, frames, 2, 1<<20)
	if len(chunks) != 1 || len(chunks[0]) != 3 {
		t.Fatalf("single flow chunks = %d/%d, want 1/3", len(chunks), len(chunks[0]))
	}
}
