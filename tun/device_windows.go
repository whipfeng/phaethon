//go:build windows

package tun

import (
	"fmt"

	"golang.zx2c4.com/wintun"
)

type windowsTUN struct {
	adapter *wintun.Adapter
	session wintun.Session
	name    string
	mtu     int
}

// CreateDevice creates a Windows Wintun device.
func CreateDevice() (Device, error) {
	const tunName = "phaethontun"
	adapter, err := wintun.CreateAdapter(tunName, "Wintun", nil)
	if err != nil {
		return nil, fmt.Errorf("create wintun adapter: %w", err)
	}

	session, err := adapter.StartSession(0x800000)
	if err != nil {
		adapter.Close()
		return nil, fmt.Errorf("start wintun session: %w", err)
	}

	return &windowsTUN{
		adapter: adapter,
		session: session,
		name:    tunName,
		mtu:     1500,
	}, nil
}

func (t *windowsTUN) Name() string { return t.name }
func (t *windowsTUN) GUID() string { return "" }
func (t *windowsTUN) MTU() int     { return t.mtu }

func (t *windowsTUN) Close() error {
	t.session.End()
	return t.adapter.Close()
}

func (t *windowsTUN) Read(buf []byte) (int, error) {
	packet, err := t.session.ReceivePacket()
	if err != nil {
		// No more data is a normal condition for non-blocking read.
		if err.Error() == "No more data is available." {
			return 0, nil
		}
		return 0, err
	}
	n := copy(buf, packet)
	t.session.ReleaseReceivePacket(packet)
	return n, nil
}

func (t *windowsTUN) Write(buf []byte) (int, error) {
	packet, err := t.session.AllocateSendPacket(len(buf))
	if err != nil {
		return 0, err
	}
	copy(packet, buf)
	t.session.SendPacket(packet)
	return len(buf), nil
}
