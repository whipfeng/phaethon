package mesh

import (
	"encoding/json"
	"time"
)

// ProbeMsg is sent periodically to measure link quality.
type ProbeMsg struct {
	Cmd       string `json:"cmd"`
	Seq       uint32 `json:"seq"`
	Timestamp int64  `json:"timestamp"` // unix nano
}

// ProbeReply is sent in response to a ProbeMsg.
type ProbeReply struct {
	Cmd       string `json:"cmd"`
	Seq       uint32 `json:"seq"`
	Timestamp int64  `json:"timestamp"` // echoed from ProbeMsg
}

// NewProbeMsg creates a new probe message.
func NewProbeMsg(seq uint32) []byte {
	msg := ProbeMsg{
		Cmd:       "probe",
		Seq:       seq,
		Timestamp: time.Now().UnixNano(),
	}
	data, _ := json.Marshal(msg)
	return data
}

// NewProbeReply creates a reply to a probe message.
func NewProbeReply(seq, timestamp int64) []byte {
	reply := ProbeReply{
		Cmd:       "probe_reply",
		Seq:       uint32(seq),
		Timestamp: timestamp,
	}
	data, _ := json.Marshal(reply)
	return data
}

// ParseProbeMsg parses a probe message.
func ParseProbeMsg(data []byte) (*ProbeMsg, error) {
	var msg ProbeMsg
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ParseProbeReply parses a probe reply message.
func ParseProbeReply(data []byte) (*ProbeReply, error) {
	var reply ProbeReply
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}
