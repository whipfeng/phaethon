package mesh

import (
	"encoding/binary"
	"testing"
)

func TestParseForwardingTraceTCPSYN(t *testing.T) {
	packet := make([]byte, 40)
	packet[0] = 0x45
	packet[9] = 6
	copy(packet[12:16], []byte{100, 0, 0, 2})
	copy(packet[16:20], []byte{100, 2, 0, 10})
	binary.BigEndian.PutUint16(packet[20:22], 43123)
	binary.BigEndian.PutUint16(packet[22:24], 443)
	binary.BigEndian.PutUint32(packet[24:28], 0x12345678)
	packet[33] = 0x02

	before := append([]byte(nil), packet...)
	trace, ok := ParseForwardingTrace(packet)
	if !ok {
		t.Fatal("expected TCP SYN trace")
	}
	if got, want := trace.Key, "tcp:100.2.0.10:443:305419896"; got != want {
		t.Fatalf("key=%q, want %q", got, want)
	}
	if trace.SrcPort != 43123 || trace.DstPort != 443 {
		t.Fatalf("ports=%d:%d", trace.SrcPort, trace.DstPort)
	}
	if string(packet) != string(before) {
		t.Fatal("ParseForwardingTrace modified packet")
	}

	packet[8] = 63
	copy(packet[12:16], []byte{100, 0, 0, 1})
	traceAfterNAT, ok := ParseForwardingTrace(packet)
	if !ok || traceAfterNAT.Key != trace.Key {
		t.Fatalf("NAT-stable key=%q, want %q", traceAfterNAT.Key, trace.Key)
	}
}

func TestParseForwardingTraceDNSAndNonTarget(t *testing.T) {
	packet := make([]byte, 40)
	packet[0] = 0x45
	packet[9] = 17
	copy(packet[12:16], []byte{100, 0, 0, 2})
	copy(packet[16:20], []byte{100, 2, 0, 3})
	binary.BigEndian.PutUint16(packet[20:22], 50000)
	binary.BigEndian.PutUint16(packet[22:24], 53)
	binary.BigEndian.PutUint16(packet[28:30], 0x4321)

	trace, ok := ParseForwardingTrace(packet)
	if !ok || trace.Key != "dns:100.2.0.3:53:17185" {
		t.Fatalf("trace=%+v ok=%t", trace, ok)
	}

	binary.BigEndian.PutUint16(packet[22:24], 5353)
	if _, ok := ParseForwardingTrace(packet); ok {
		t.Fatal("non-DNS UDP packet should not be traced")
	}
}
