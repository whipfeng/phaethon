package mesh

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync/atomic"

	"phaethon/util"
)

var forwardingTrace atomic.Bool

type ForwardingTrace struct {
	Key     string
	ReplyTo string
	Kind    string
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
	Seq     uint32
	Ack     uint32
	Flags   uint8
}

func SetForwardingTrace(enabled bool) {
	forwardingTrace.Store(enabled)
}

func ForwardingTraceEnabled() bool {
	return forwardingTrace.Load()
}

func ParseForwardingTrace(data []byte) (ForwardingTrace, bool) {
	if len(data) < 20 || data[0]>>4 != 4 {
		return ForwardingTrace{}, false
	}

	headerLen := int(data[0]&0x0f) * 4
	if headerLen < 20 || len(data) < headerLen {
		return ForwardingTrace{}, false
	}

	trace := ForwardingTrace{
		SrcIP: net.IP(data[12:16]).To4(),
		DstIP: net.IP(data[16:20]).To4(),
	}

	switch data[9] {
	case 6:
		if len(data) < headerLen+20 {
			return ForwardingTrace{}, false
		}
		tcp := data[headerLen:]
		tcpHeaderLen := int(tcp[12]>>4) * 4
		if tcpHeaderLen < 20 || len(tcp) < tcpHeaderLen {
			return ForwardingTrace{}, false
		}
		trace.SrcPort = binary.BigEndian.Uint16(tcp[:2])
		trace.DstPort = binary.BigEndian.Uint16(tcp[2:4])
		trace.Seq = binary.BigEndian.Uint32(tcp[4:8])
		trace.Ack = binary.BigEndian.Uint32(tcp[8:12])
		trace.Flags = tcp[13]
		trace.Key = fmt.Sprintf("tcp:%s:%d:%d", trace.DstIP, trace.DstPort, trace.Seq)

		switch {
		case trace.Flags&0x02 != 0 && trace.Flags&0x10 == 0:
			trace.Kind = "tcp_syn"
		case trace.Flags&0x12 == 0x12:
			trace.Kind = "tcp_syn_ack"
			trace.ReplyTo = fmt.Sprintf("tcp:%s:%d:%d", trace.SrcIP, trace.SrcPort, trace.Ack-1)
		case trace.Flags&0x01 != 0:
			trace.Kind = "tcp_fin"
		case trace.Flags&0x04 != 0:
			trace.Kind = "tcp_rst"
		case len(tcp) > tcpHeaderLen:
			trace.Kind = "tcp_data"
		default:
			trace.Kind = "tcp_ack"
		}
		return trace, true
	case 17:
		if len(data) < headerLen+12 {
			return ForwardingTrace{}, false
		}
		udp := data[headerLen:]
		trace.SrcPort = binary.BigEndian.Uint16(udp[:2])
		trace.DstPort = binary.BigEndian.Uint16(udp[2:4])
		if trace.DstPort != 53 {
			return ForwardingTrace{}, false
		}
		trace.Kind = "dns"
		id := binary.BigEndian.Uint16(udp[8:10])
		trace.Key = fmt.Sprintf("dns:%s:%d:%d", trace.DstIP, trace.DstPort, id)
		return trace, true
	default:
		return ForwardingTrace{}, false
	}
}

func TraceForwarding(stage string, data []byte, format string, args ...interface{}) {
	if !ForwardingTraceEnabled() {
		return
	}
	trace, ok := ParseForwardingTrace(data)
	if !ok {
		return
	}
	prefix := fmt.Sprintf("[MESH-TRACE] stage=%s key=%s kind=%s src=%s:%d dst=%s:%d", stage, trace.Key, trace.Kind, trace.SrcIP, trace.SrcPort, trace.DstIP, trace.DstPort)
	if trace.Kind != "dns" {
		prefix += fmt.Sprintf(" seq=%d ack=%d flags=0x%02x", trace.Seq, trace.Ack, trace.Flags)
		if trace.ReplyTo != "" {
			prefix += fmt.Sprintf(" reply_to=%s", trace.ReplyTo)
		}
	}
	if format == "" {
		util.LogInfo("%s", prefix)
		return
	}
	util.LogInfo(prefix+" "+format, args...)
}

func TraceForwardingRoute(dst net.IP, format string, args ...interface{}) {
	if !ForwardingTraceEnabled() {
		return
	}
	util.LogInfo("[MESH-TRACE] stage=route_decision dst=%s "+format, append([]interface{}{dst}, args...)...)
}
