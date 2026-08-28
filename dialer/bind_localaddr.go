package dialer

import (
	"net"

	"phaethon/util"
)

// selectLocalIP selects the best source IP from the interface identified by
// ifIndex for traffic to dst, following RFC 6724 address selection rules to
// stay consistent with the OS's native source address selection.
//
// Rules (in priority order):
//  1. Address family match: IPv4 dst → IPv4 addr, IPv6 dst → IPv6 addr.
//     dst=nil → prefer IPv4.
//  2. Exclude link-local, loopback.
//  3. Prefer same-subnet address (longest prefix match with dst).
//  4. Prefer longest common prefix with dst (RFC 6724 Rule 9).
//  5. If all equal, take the first match.
//  6. If no match, return nil (caller should skip binding).
func selectLocalIP(ifIndex int, dst net.IP) net.IP {
	iface, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		util.LogWarn("dialer/bind: interface %d not found: %v", ifIndex, err)
		return nil
	}

	addrs, err := iface.Addrs()
	if err != nil {
		util.LogWarn("dialer/bind: failed to get addresses for interface %d: %v", ifIndex, err)
		return nil
	}

	wantV4 := dst == nil || dst.To4() != nil

	var bestIP net.IP
	bestPrefix := -1

	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP

		isV4 := ip.To4() != nil
		if wantV4 != isV4 {
			continue
		}

		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsLoopback() {
			continue
		}

		prefixLen := commonPrefixLen(ip, dst)

		if prefixLen > bestPrefix {
			bestPrefix = prefixLen
			bestIP = ip
		}
	}

	if bestIP == nil {
		util.LogWarn("dialer/bind: no suitable IP on interface %d for dst %s", ifIndex, dst)
		return nil
	}

	return bestIP
}

// commonPrefixLen returns the number of leading bits that ip and dst share.
// If dst is nil, returns 0 (no preference).
func commonPrefixLen(ip, dst net.IP) int {
	if dst == nil {
		return 0
	}

	ip4 := ip.To4()
	dst4 := dst.To4()
	if ip4 != nil && dst4 != nil {
		return commonPrefixLenBytes(ip4, dst4)
	}

	ip16 := ip.To16()
	dst16 := dst.To16()
	if ip16 != nil && dst16 != nil {
		return commonPrefixLenBytes(ip16, dst16)
	}

	return 0
}

func commonPrefixLenBytes(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	count := 0
	for i := 0; i < n; i++ {
		xor := a[i] ^ b[i]
		if xor == 0 {
			count += 8
			continue
		}
		for bit := 7; bit >= 0; bit-- {
			if xor&(1<<bit) == 0 {
				count++
			} else {
				return count
			}
		}
	}
	return count
}
