//go:build !linux

package tun

// Non-Linux platforms manage their TUN adapter lifecycle differently (Wintun
// adapter removal / utun fd close), so there is no owner marker to keep.
func recordTUNOwner(string, string) {}

func clearTUNOwner() {}

func readTUNOwner() (tunOwnerInfo, bool) { return tunOwnerInfo{}, false }
