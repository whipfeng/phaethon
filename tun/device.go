package tun

// Device abstracts a TUN/TAP network interface.
type Device interface {
	Name() string
	GUID() string
	Read(buf []byte) (int, error)
	Write(buf []byte) (int, error)
	Close() error
	MTU() int
}
