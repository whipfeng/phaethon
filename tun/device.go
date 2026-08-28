package tun

import "errors"

// ErrSessionClosed is returned by Device.Read when the underlying TUN session
// has ended (e.g. the adapter was removed). It signals the read loop to exit.
var ErrSessionClosed = errors.New("tun session closed")

// Device abstracts a TUN/TAP network interface.
type Device interface {
	Name() string
	GUID() string
	Read(buf []byte) (int, error)
	Write(buf []byte) (int, error)
	Close() error
	MTU() int
}
