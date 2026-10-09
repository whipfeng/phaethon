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
	Kind    string
	SrcIP   net.IP
	DstIP   net.IP
	SrcPort uint16
	DstPort uint16
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
		if tcp[13]&0x02 == 0 || tcp[13]&0x10 != 0 {
			return ForwardingTrace{}, false
		}
		trace.Kind = "tcp_syn"
		trace.SrcPort = binary.BigEndian.Uint16(tcp[:2])
		trace.DstPort = binary.BigEndian.Uint16(tcp[2:4])
		seq := binary.BigEndian.Uint32(tcp[4:8])
		trace.Key = fmt.Sprintf("tcp:%s:%d:%d", trace.DstIP, trace.DstPort, seq)
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
