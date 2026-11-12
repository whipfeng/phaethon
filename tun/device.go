package tun

import "errors"

// ErrSessionClosed is returned by Device.Read when the underlying TUN session
// has ended (e.g. the adapter was removed). It signals the read loop to exit.
var ErrSessionClosed = errors.New("tun session closed")

// OverlayMTU is the inner-packet MTU of the mesh overlay, applied to the TUN
// adapter on every platform.
//
// It is sized so a mesh frame fits a 1500-byte path without IP fragmentation
// when carried by the UDP leg:
//
//	inner 1400 + IPIP outer 20 + frame header 3 + UDP 8 + outer IP 20 = 1451
//
// 49 bytes of headroom remain (e.g. PPPoE/VLAN paths). Carrying ~1500-byte
// inner packets made every datagram ~1548 bytes, i.e. two IP fragments; one
// lost fragment discarded the whole datagram (a full inner TCP segment) and the
// inner TCP collapsed into multi-second RTO stalls. Sizing the tunnel MTU to
// the path MTU keeps each datagram whole and the flow on one ordered carrier.
const OverlayMTU = 1400

// Device abstracts a TUN/TAP network interface.
type Device interface {
	Name() string
	GUID() string
	Read(buf []byte) (int, error)
	Write(buf []byte) (int, error)
	Close() error
	MTU() int
}
