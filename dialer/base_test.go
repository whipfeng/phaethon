package dialer

import (
	"testing"

	"phaethon/config"
)

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
