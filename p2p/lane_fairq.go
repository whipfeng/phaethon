package p2p

import (
	"sync/atomic"

	"phaethon/frame"
)

// Lane-level per-flow fair queueing (lab). When enabled, a multi-flow batch is
// split into per-flow chunks and round-robined before entering the lane FIFO,
// so two inner TCP flows sharing a lane alternate at chunk granularity instead
// of one flow's whole batch draining first. Flow order within a chunk is
// preserved; no live flow is moved across lanes.

const (
	laneFairQFrames = 8
	laneFairQBytes  = 16 << 10
)

var laneFairQ atomic.Bool

func SetLaneFairQEnabled(on bool) { laneFairQ.Store(on) }
func LaneFairQEnabled() bool      { return laneFairQ.Load() }

// fairLaneBatches groups frames by inner flow and emits round-robin chunks.
// A single-flow batch is returned unchanged.
func fairLaneBatches(salt uint32, frames []frame.Frame, maxFrames, maxBytes int) [][]frame.Frame {
	if len(frames) <= 1 {
		return [][]frame.Frame{frames}
	}
	type group struct {
		frames []frame.Frame
	}
	order := make([]uint32, 0, 4)
	groups := make(map[uint32]*group, 4)
	for _, f := range frames {
		k := flowHash32(salt, f.Payload)
		g := groups[k]
		if g == nil {
			g = &group{}
			groups[k] = g
			order = append(order, k)
		}
		g.frames = append(g.frames, f)
	}
	if len(order) <= 1 {
		return [][]frame.Frame{frames}
	}
	idx := make(map[uint32]int, len(order))
	var out [][]frame.Frame
	for {
		progress := false
		for _, k := range order {
			g := groups[k]
			i := idx[k]
			if i >= len(g.frames) {
				continue
			}
			end := i
			bytes := 0
			for end < len(g.frames) && end-i < maxFrames {
				n := len(g.frames[end].Payload)
				if end > i && bytes+n > maxBytes {
					break
				}
				bytes += n
				end++
			}
			out = append(out, g.frames[i:end])
			idx[k] = end
			progress = true
		}
		if !progress {
			break
		}
	}
	return out
}
