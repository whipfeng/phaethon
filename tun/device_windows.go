//go:build windows

package tun

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wintun"
)

type windowsTUN struct {
	adapter  *wintun.Adapter
	session  wintun.Session
	readWait windows.Handle
	name     string
	mtu      int

	close   atomic.Bool
	running sync.WaitGroup
	closeMu sync.Once
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

	readWait := session.ReadWaitEvent()

	return &windowsTUN{
		adapter:  adapter,
		session:  session,
		readWait: readWait,
		name:     tunName,
		mtu:      1500,
	}, nil
}

func (t *windowsTUN) Name() string { return t.name }
func (t *windowsTUN) GUID() string { return "" }
func (t *windowsTUN) MTU() int     { return t.mtu }

// LUID returns the Wintun adapter's interface LUID. It is available immediately
// after CreateAdapter and avoids the retry loop otherwise needed for the TCP/IP
// stack to register the adapter name.
func (t *windowsTUN) LUID() uint64 {
	return t.adapter.LUID()
}

func (t *windowsTUN) Close() error {
	t.closeMu.Do(func() {
		t.close.Store(true)
		windows.SetEvent(t.readWait)
		t.running.Wait()
		t.session.End()
		t.adapter.Close()
	})
	return nil
}

func (t *windowsTUN) Read(buf []byte) (int, error) {
	t.running.Add(1)
	defer t.running.Done()

	for {
		if t.close.Load() {
			return 0, ErrSessionClosed
		}

		packet, err := t.session.ReceivePacket()
		switch {
		case err == nil:
			n := copy(buf, packet)
			t.session.ReleaseReceivePacket(packet)
			return n, nil
		case errors.Is(err, windows.ERROR_NO_MORE_ITEMS):
			waitResult, waitErr := windows.WaitForSingleObject(t.readWait, windows.INFINITE)
			if waitErr != nil {
				return 0, fmt.Errorf("wait for read event: %w", waitErr)
			}
			if waitResult == windows.WAIT_FAILED {
				return 0, fmt.Errorf("wait for read event failed")
			}
			continue
		case errors.Is(err, windows.ERROR_HANDLE_EOF):
			return 0, ErrSessionClosed
		default:
			return 0, err
		}
	}
}

func (t *windowsTUN) Write(buf []byte) (int, error) {
	t.running.Add(1)
	defer t.running.Done()

	if t.close.Load() {
		return 0, ErrSessionClosed
	}

	packet, err := t.session.AllocateSendPacket(len(buf))
	if err != nil {
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			return len(buf), nil
		}
		return 0, err
	}
	copy(packet, buf)
	t.session.SendPacket(packet)
	return len(buf), nil
}
