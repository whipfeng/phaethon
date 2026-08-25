package dialer

import (
	"testing"

	"phaethon/config"
)

func TestBaseDialer_ResolveCmd(t *testing.T) {
	d := BaseDialer{Proxy: &config.Proxy{}}

	// Auto-detect: dstPort==0 -> BIND
	if got := d.ResolveCmd(0); got != 0x02 {
		t.Errorf("ResolveCmd(0) = 0x%02x, want BIND(0x02)", got)
	}
	// Auto-detect: dstPort!=0 -> CONNECT
	if got := d.ResolveCmd(8080); got != 0x01 {
		t.Errorf("ResolveCmd(8080) = 0x%02x, want CONNECT(0x01)", got)
	}

	// Explicit CmdType overrides auto-detect
	d.CmdType = 0x02
	if got := d.ResolveCmd(8080); got != 0x02 {
		t.Errorf("explicit BIND: ResolveCmd(8080) = 0x%02x, want BIND(0x02)", got)
	}

	d.CmdType = 0x01
	if got := d.ResolveCmd(0); got != 0x01 {
		t.Errorf("explicit CONNECT: ResolveCmd(0) = 0x%02x, want CONNECT(0x01)", got)
	}
}

func TestBaseDialer_IsBind(t *testing.T) {
	d := BaseDialer{Proxy: &config.Proxy{}}

	if d.IsBind(0) != true {
		t.Error("IsBind(0) should be true")
	}
	if d.IsBind(8080) != false {
		t.Error("IsBind(8080) should be false")
	}

	d.CmdType = 0x01
	if d.IsBind(0) != false {
		t.Error("explicit CONNECT: IsBind(0) should be false")
	}
}

func TestBaseDialer_TryReverse_NoReverseAddress(t *testing.T) {
	d := BaseDialer{Proxy: &config.Proxy{ReverseAddress: ""}}
	conn, err := d.TryReverse()
	if err != nil {
		t.Errorf("TryReverse with empty address should not error, got: %v", err)
	}
	if conn != nil {
		t.Error("TryReverse with empty address should return nil conn")
	}
}
