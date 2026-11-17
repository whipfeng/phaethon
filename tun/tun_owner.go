package tun

// tunOwnerInfo is the persisted proof that a TUN interface belongs to this
// phaethon instance. instance is the data directory (one deployment), mac is
// the interface's hardware address at creation time (guards against another
// tun* device reusing the same name after a crash).
type tunOwnerInfo struct {
	iface    string
	instance string
	mac      string
	alias    string
}
