//go:build linux

package tun

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"phaethon/config"
	"phaethon/util"
	"sort"
	"sync"
	"syscall"
	"time"
)

// DHCP message types (option 53)
const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpDecline  = 4
	dhcpACK      = 5
	dhcpNAK      = 6
	dhcpRelease  = 7
)

// DHCP option codes
const (
	optSubnetMask       = 1
	optRouter           = 3
	optDNS              = 6
	optHostName         = 12
	optDomainName       = 15
	optRequestedIP      = 50
	optLeaseTime        = 51
	optMessageType      = 53
	optServerIdentifier = 54
	optRenewalTime      = 58
	optRebindingTime    = 59
	optClientIdentifier = 61
	optEnd              = 255
)

// DHCP magic cookie
var dhcpMagic = [4]byte{99, 130, 83, 99}

// dhcpMessage is a raw DHCP message (fixed 236 bytes header + variable options).
type dhcpMessage struct {
	op      byte
	htype   byte
	hlen    byte
	hops    byte
	xid     [4]byte
	secs    [2]byte
	flags   [2]byte
	ciaddr  [4]byte
	yiaddr  [4]byte
	siaddr  [4]byte
	giaddr  [4]byte
	chaddr  [16]byte
	sname   [64]byte
	file    [128]byte
	options map[byte][]byte
}

func parseDHCPMessage(data []byte) (*dhcpMessage, error) {
	if len(data) < 240 {
		return nil, fmt.Errorf("dhcp: message too short: %d bytes", len(data))
	}
	m := &dhcpMessage{
		op:      data[0],
		htype:   data[1],
		hlen:    data[2],
		hops:    data[3],
		options: make(map[byte][]byte),
	}
	copy(m.xid[:], data[4:8])
	copy(m.secs[:], data[8:10])
	copy(m.flags[:], data[10:12])
	copy(m.ciaddr[:], data[12:16])
	copy(m.yiaddr[:], data[16:20])
	copy(m.siaddr[:], data[20:24])
	copy(m.giaddr[:], data[24:28])
	copy(m.chaddr[:], data[28:44])
	copy(m.sname[:], data[44:108])
	copy(m.file[:], data[108:236])

	// Verify magic cookie
	if data[236] != 99 || data[237] != 130 || data[238] != 83 || data[239] != 99 {
		return nil, fmt.Errorf("dhcp: invalid magic cookie")
	}

	// Parse options
	i := 240
	for i < len(data) {
		opt := data[i]
		if opt == optEnd {
			break
		}
		if opt == 0 { // padding
			i++
			continue
		}
		if i+1 >= len(data) {
			break
		}
		length := int(data[i+1])
		if i+2+length > len(data) {
			break
		}
		optData := make([]byte, length)
		copy(optData, data[i+2:i+2+length])
		m.options[opt] = optData
		i += 2 + length
	}
	return m, nil
}

func (m *dhcpMessage) messageType() byte {
	if opt, ok := m.options[optMessageType]; ok && len(opt) == 1 {
		return opt[0]
	}
	return 0
}

func (m *dhcpMessage) clientMAC() net.HardwareAddr {
	hlen := int(m.hlen)
	if hlen > 16 {
		hlen = 16
	}
	return net.HardwareAddr(m.chaddr[:hlen])
}

func (m *dhcpMessage) requestedIP() net.IP {
	if opt, ok := m.options[optRequestedIP]; ok && len(opt) == 4 {
		return net.IP(opt)
	}
	return nil
}

func (m *dhcpMessage) hostName() string {
	if opt, ok := m.options[optHostName]; ok {
		return string(opt)
	}
	return ""
}

func (m *dhcpMessage) serialize() []byte {
	buf := make([]byte, 236, 512)
	buf[0] = m.op
	buf[1] = m.htype
	buf[2] = m.hlen
	buf[3] = m.hops
	copy(buf[4:8], m.xid[:])
	copy(buf[8:10], m.secs[:])
	copy(buf[10:12], m.flags[:])
	copy(buf[12:16], m.ciaddr[:])
	copy(buf[16:20], m.yiaddr[:])
	copy(buf[20:24], m.siaddr[:])
	copy(buf[24:28], m.giaddr[:])
	copy(buf[28:44], m.chaddr[:])
	copy(buf[44:108], m.sname[:])
	copy(buf[108:236], m.file[:])

	// Magic cookie
	buf = append(buf, 99, 130, 83, 99)

	// Output options in deterministic order: Message Type first, then numerical order
	// RFC 2132 requires option 53 (Message Type) to be first
	if data, ok := m.options[optMessageType]; ok {
		buf = append(buf, optMessageType, byte(len(data)))
		buf = append(buf, data...)
	}

	// Collect and sort remaining option codes
	codes := make([]byte, 0, len(m.options)-1)
	for code := range m.options {
		if code != optMessageType {
			codes = append(codes, code)
		}
	}
	// Simple insertion sort (small number of options)
	for i := 1; i < len(codes); i++ {
		key := codes[i]
		j := i - 1
		for j >= 0 && codes[j] > key {
			codes[j+1] = codes[j]
			j--
		}
		codes[j+1] = key
	}

	for _, code := range codes {
		data := m.options[code]
		buf = append(buf, code, byte(len(data)))
		buf = append(buf, data...)
	}
	buf = append(buf, optEnd)
	return buf
}

// lease tracks a single DHCP lease.
type lease struct {
	ip       net.IP
	mac      net.HardwareAddr
	hostname string
	expires  time.Time
}

// dhcpServerImpl is the Linux DHCP server implementation.
type dhcpServerImpl struct {
	ifaceName string
	cfg       *config.DHCPConfig
	dnsAddr   net.IP
	dataDir   string
	leaseFile string

	rawFD    int // AF_PACKET socket for sending
	ifIndex  int
	ifHWAddr net.HardwareAddr

	conn *net.UDPConn // UDP socket for receiving only

	mu       sync.Mutex
	leases   map[string]*lease // MAC string -> lease
	poolNext net.IP
	poolEnd  net.IP
	leaseDur time.Duration

	staticBindings map[string]net.IP // MAC string -> static IP

	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newDHCPServerImpl(ifaceName string, cfg *config.DHCPConfig, dnsAddr net.IP, dataDir string) (DHCPServer, error) {
	if ifaceName == "" {
		return nil, fmt.Errorf("dhcp: interface name is empty")
	}
	if cfg == nil {
		return nil, fmt.Errorf("dhcp: config is nil")
	}

	// Get interface info for pool auto-generation
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("dhcp: interface %q: %w", ifaceName, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("dhcp: get interface addrs: %w", err)
	}

	var ifaceIP net.IP
	var ifaceMask net.IPMask
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
			ifaceIP = ipNet.IP.To4()
			ifaceMask = ipNet.Mask
			break
		}
	}
	if ifaceIP == nil {
		return nil, fmt.Errorf("dhcp: no IPv4 address on interface %q", ifaceName)
	}

	// Determine pool range: use configured values or auto-generate
	var poolStart, poolEnd net.IP
	if cfg.PoolStart != "" && cfg.PoolEnd != "" {
		poolStart = net.ParseIP(cfg.PoolStart).To4()
		poolEnd = net.ParseIP(cfg.PoolEnd).To4()
		if poolStart == nil || poolEnd == nil {
			return nil, fmt.Errorf("dhcp: invalid pool-start or pool-end")
		}
	} else {
		// Auto-generate pool from interface subnet
		// Use the upper portion of the subnet: .200 - .250 for /24
		// For smaller subnets, use the upper half
		networkIP := ifaceIP.Mask(ifaceMask)
		ones, bits := ifaceMask.Size()
		hostBits := bits - ones
		totalHosts := uint32(1) << uint(hostBits)

		// Calculate pool: use last 50 IPs or half the subnet if smaller
		poolSize := uint32(50)
		if totalHosts < 100 {
			poolSize = totalHosts / 2
		}
		if poolSize < 10 {
			poolSize = 10
		}

		// Pool ends at broadcast - 1 (last usable IP)
		broadcastIP := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			broadcastIP[i] = networkIP[i] | ^ifaceMask[i]
		}
		endUint32 := dhcpIPToUint32(broadcastIP) - 1
		startUint32 := endUint32 - poolSize + 1

		poolStart = dhcpUint32ToIP(startUint32)
		poolEnd = dhcpUint32ToIP(endUint32)

		util.LogInfo("dhcp: auto-generated pool %s-%s from %s/%d", poolStart, poolEnd, networkIP, ones)
	}

	s := &dhcpServerImpl{
		ifaceName:      ifaceName,
		cfg:            cfg,
		dnsAddr:        dnsAddr.To4(),
		dataDir:        dataDir,
		leases:         make(map[string]*lease),
		poolNext:       poolStart,
		poolEnd:        poolEnd,
		leaseDur:       cfg.LeaseDuration(),
		staticBindings: make(map[string]net.IP),
		stopCh:         make(chan struct{}),
	}

	for _, b := range cfg.StaticBindings {
		mac := normalizeMAC(b.MAC)
		if ip := net.ParseIP(b.IP); ip != nil && mac != "" {
			s.staticBindings[mac] = ip.To4()
		}
	}

	if dataDir != "" {
		s.leaseFile = filepath.Join(dataDir, "dhcp-leases.json")
		s.loadLeases()
	}

	return s, nil
}

func (s *dhcpServerImpl) Start() error {
	iface, err := net.InterfaceByName(s.ifaceName)
	if err != nil {
		return fmt.Errorf("dhcp: interface %q: %w", s.ifaceName, err)
	}
	s.ifIndex = iface.Index
	s.ifHWAddr = iface.HardwareAddr

	// UDP socket for receiving DHCP requests
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			err := c.Control(func(fd uintptr) {
				if err := syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, s.ifaceName); err != nil {
					opErr = fmt.Errorf("SO_BINDTODEVICE %q: %w", s.ifaceName, err)
					return
				}
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
				_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
			})
			if err != nil {
				return err
			}
			return opErr
		},
	}

	conn, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:67")
	if err != nil {
		return fmt.Errorf("dhcp: listen :67: %w", err)
	}
	s.conn = conn.(*net.UDPConn)

	// AF_PACKET socket for sending DHCP replies with full control over checksums
	rawFD, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(syscall.ETH_P_IP)))
	if err != nil {
		s.conn.Close()
		return fmt.Errorf("dhcp: AF_PACKET socket: %w", err)
	}
	s.rawFD = rawFD

	// Bind to the interface
	sll := &syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_IP),
		Ifindex:  s.ifIndex,
	}
	if err := syscall.Bind(rawFD, sll); err != nil {
		syscall.Close(rawFD)
		s.conn.Close()
		return fmt.Errorf("dhcp: AF_PACKET bind: %w", err)
	}

	util.LogInfo("dhcp: AF_PACKET socket fd=%d ifindex=%d mac=%s", rawFD, s.ifIndex, s.ifHWAddr)

	gw, _, _, _ := s.getIfaceInfo()
	util.LogInfo("dhcp: listening on %s (gateway=%s, dns=%s, pool=%s-%s)",
		s.ifaceName, gw, s.dnsAddr, s.poolNext, s.poolEnd)

	s.wg.Add(1)
	go s.serve()
	return nil
}

type leaseJSON struct {
	IP       string    `json:"ip"`
	MAC      string    `json:"mac"`
	Hostname string    `json:"hostname,omitempty"`
	Expires  time.Time `json:"expires"`
}

func (s *dhcpServerImpl) loadLeases() {
	data, err := os.ReadFile(s.leaseFile)
	if err != nil {
		return // file doesn't exist yet, that's fine
	}
	var entries []leaseJSON
	if err := json.Unmarshal(data, &entries); err != nil {
		util.LogWarn("dhcp: failed to parse lease file: %v", err)
		return
	}
	now := time.Now()
	count := 0
	for _, e := range entries {
		if e.Expires.Before(now) {
			continue // expired
		}
		ip := net.ParseIP(e.IP).To4()
		if ip == nil {
			continue
		}
		mac, _ := net.ParseMAC(e.MAC)
		s.leases[e.MAC] = &lease{
			ip:       ip,
			mac:      mac,
			hostname: e.Hostname,
			expires:  e.Expires,
		}
		count++
	}
	if count > 0 {
		util.LogInfo("dhcp: loaded %d leases from %s", count, s.leaseFile)
	}
}

func (s *dhcpServerImpl) saveLeases() {
	if s.leaseFile == "" {
		return
	}
	s.mu.Lock()
	entries := make([]leaseJSON, 0, len(s.leases))
	now := time.Now()
	for mac, l := range s.leases {
		if l.expires.After(now) {
			entries = append(entries, leaseJSON{
				IP:       l.ip.String(),
				MAC:      mac,
				Hostname: l.hostname,
				Expires:  l.expires,
			})
		}
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		util.LogWarn("dhcp: failed to marshal leases: %v", err)
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.leaseFile), 0755)
	if err := os.WriteFile(s.leaseFile, data, 0644); err != nil {
		util.LogWarn("dhcp: failed to write leases: %v", err)
	} else {
		util.DefaultVersionNotifier.BumpVersion("tun")
	}
}

func (s *dhcpServerImpl) Stop() {
	s.saveLeases()
	close(s.stopCh)
	if s.conn != nil {
		s.conn.Close()
	}
	if s.rawFD != 0 {
		syscall.Close(s.rawFD)
	}
	s.wg.Wait()
	util.LogInfo("dhcp: stopped")
}

func (s *dhcpServerImpl) ActiveLeases() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	count := 0
	for _, l := range s.leases {
		if l.expires.After(now) {
			count++
		}
	}
	return count
}

func (s *dhcpServerImpl) Leases() []DHCPLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]DHCPLease, 0, len(s.leases))
	for _, l := range s.leases {
		if l.expires.After(now) {
			out = append(out, DHCPLease{
				IP:       l.ip.String(),
				MAC:      l.mac.String(),
				Hostname: l.hostname,
				Expires:  l.expires,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a := net.ParseIP(out[i].IP).To4()
		b := net.ParseIP(out[j].IP).To4()
		return bytes.Compare(a, b) < 0
	})
	return out
}

// getIfaceInfo returns the current IPv4 address, mask, and MAC of the bound interface.
func (s *dhcpServerImpl) getIfaceInfo() (ip net.IP, mask net.IPMask, mac net.HardwareAddr, err error) {
	iface, err := net.InterfaceByName(s.ifaceName)
	if err != nil {
		return nil, nil, nil, err
	}
	mac = iface.HardwareAddr
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, nil, nil, err
	}
	for _, a := range addrs {
		if ipNet, ok := a.(*net.IPNet); ok && ipNet.IP.To4() != nil {
			return ipNet.IP.To4(), ipNet.Mask, mac, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("no IPv4 address on %s", s.ifaceName)
}

func (s *dhcpServerImpl) serve() {
	defer s.wg.Done()
	buf := make([]byte, 1500)
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		s.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remoteAddr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			select {
			case <-s.stopCh:
				return
			default:
				util.LogWarn("dhcp: read error: %v", err)
				continue
			}
		}

		msg, err := parseDHCPMessage(buf[:n])
		if err != nil {
			util.LogWarn("dhcp: parse error: %v", err)
			continue
		}
		if msg.op != 1 { // not a BOOTREQUEST
			continue
		}

		s.handleMessage(msg, remoteAddr)
	}
}

func (s *dhcpServerImpl) handleMessage(req *dhcpMessage, from *net.UDPAddr) {
	msgType := req.messageType()
	mac := req.clientMAC()
	macStr := mac.String()
	hostname := req.hostName()

	// Ignore our own DHCP requests (if this interface is also a DHCP client).
	_, _, ifaceMAC, err := s.getIfaceInfo()
	if err == nil && ifaceMAC != nil && mac.String() == ifaceMAC.String() {
		return
	}

	switch msgType {
	case dhcpDiscover:
		s.mu.Lock()
		offeredIP := s.allocateIP(macStr)
		if offeredIP != nil {
			s.leases[macStr] = &lease{
				ip:       offeredIP,
				mac:      mac,
				hostname: hostname,
				expires:  time.Now().Add(s.leaseDur),
			}
		}
		s.mu.Unlock()

		if offeredIP == nil {
			util.LogWarn("dhcp: pool exhausted for %s", macStr)
			return
		}
		s.sendReply(dhcpOffer, req, offeredIP)
		s.saveLeases()
		util.LogInfo("dhcp: OFFER %s -> %s", macStr, offeredIP)

	case dhcpRequest:
		reqIP := req.requestedIP()
		if reqIP == nil {
			reqIP = net.IP(req.ciaddr[:])
			if reqIP.Equal(net.IPv4zero) {
				return
			}
		}

		s.mu.Lock()
		// Static binding overrides requested IP
		if staticIP, ok := s.staticBindings[macStr]; ok {
			reqIP = staticIP
		}
		if existing, ok := s.leases[macStr]; ok && existing.ip.Equal(reqIP) && existing.expires.After(time.Now()) {
			existing.expires = time.Now().Add(s.leaseDur)
			if hostname != "" {
				existing.hostname = hostname
			}
			s.mu.Unlock()
			s.sendReply(dhcpACK, req, reqIP)
			s.saveLeases()
			util.LogInfo("dhcp: ACK (renew) %s -> %s", macStr, reqIP)
		} else {
			s.leases[macStr] = &lease{
				ip:       reqIP,
				mac:      mac,
				hostname: hostname,
				expires:  time.Now().Add(s.leaseDur),
			}
			s.mu.Unlock()
			s.sendReply(dhcpACK, req, reqIP)
			s.saveLeases()
			util.LogInfo("dhcp: ACK %s -> %s", macStr, reqIP)
		}

	case dhcpRelease:
		s.mu.Lock()
		delete(s.leases, macStr)
		s.mu.Unlock()
		util.LogInfo("dhcp: RELEASE %s", macStr)

	case dhcpDecline:
		s.mu.Lock()
		delete(s.leases, macStr)
		s.mu.Unlock()
		util.LogWarn("dhcp: DECLINE %s", macStr)
	}
}

func (s *dhcpServerImpl) allocateIP(macStr string) net.IP {
	now := time.Now()

	// Static binding takes priority over everything
	if staticIP, ok := s.staticBindings[macStr]; ok {
		conflict := false
		for mac, l := range s.leases {
			if mac != macStr && l.ip.Equal(staticIP) && l.expires.After(now) {
				conflict = true
				break
			}
		}
		if !conflict {
			s.leases[macStr] = &lease{
				ip:      staticIP,
				mac:     net.HardwareAddr{},
				expires: now.Add(s.leaseDur),
			}
			return staticIP
		}
	}

	if existing, ok := s.leases[macStr]; ok && existing.expires.After(now) {
		return existing.ip
	}
	startIP := make(net.IP, len(s.poolNext))
	copy(startIP, s.poolNext)

	for {
		candidate := make(net.IP, len(s.poolNext))
		copy(candidate, s.poolNext)

		inUse := false
		for _, l := range s.leases {
			if l.ip.Equal(candidate) && l.expires.After(now) {
				inUse = true
				break
			}
		}
		if !inUse {
			for _, sip := range s.staticBindings {
				if sip.Equal(candidate) {
					inUse = true
					break
				}
			}
		}

		s.advancePool()

		if !inUse {
			s.leases[macStr] = &lease{
				ip:      candidate,
				mac:     net.HardwareAddr{},
				expires: now.Add(s.leaseDur),
			}
			return candidate
		}

		if s.poolNext.Equal(startIP) {
			return nil
		}
	}
}

func (s *dhcpServerImpl) advancePool() {
	ip := make(net.IP, len(s.poolNext))
	copy(ip, s.poolNext)
	for i := 3; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
	if dhcpIPToUint32(ip) > dhcpIPToUint32(s.poolEnd) {
		poolStart := net.ParseIP(s.cfg.PoolStart).To4()
		copy(s.poolNext, poolStart)
	} else {
		copy(s.poolNext, ip)
	}
}

func (s *dhcpServerImpl) sendReply(msgType byte, req *dhcpMessage, assignedIP net.IP) {
	gateway, mask, _, err := s.getIfaceInfo()
	if err != nil {
		util.LogWarn("dhcp: failed to get interface info: %v", err)
		return
	}

	reply := &dhcpMessage{
		op:      2, // BOOTREPLY
		htype:   req.htype,
		hlen:    req.hlen,
		hops:    0,
		xid:     req.xid,
		flags:   req.flags,
		giaddr:  req.giaddr,
		chaddr:  req.chaddr,
		options: make(map[byte][]byte),
	}

	assigned4 := assignedIP.To4()
	copy(reply.yiaddr[:], assigned4)
	copy(reply.siaddr[:], gateway)

	leaseSecs := uint32(s.leaseDur.Seconds())

	reply.options[optMessageType] = []byte{msgType}
	reply.options[optServerIdentifier] = gateway
	lt := make([]byte, 4)
	binary.BigEndian.PutUint32(lt, leaseSecs)
	reply.options[optLeaseTime] = lt
	reply.options[optSubnetMask] = []byte(mask)
	reply.options[optRouter] = gateway
	reply.options[optDNS] = s.dnsAddr.To4()
	reply.options[optDomainName] = []byte("phaethon.local")
	t1 := make([]byte, 4)
	binary.BigEndian.PutUint32(t1, leaseSecs/2)
	reply.options[optRenewalTime] = t1
	t2 := make([]byte, 4)
	binary.BigEndian.PutUint32(t2, leaseSecs*7/8)
	reply.options[optRebindingTime] = t2
	if cid, ok := req.options[optClientIdentifier]; ok {
		reply.options[optClientIdentifier] = cid
	}

	dhcpData := reply.serialize()

	// Build UDP packet
	srcIP := gateway.To4()

	// Always broadcast DHCP replies (OFFER/ACK) to 255.255.255.255
	dstIP := net.IPv4bcast.To4()

	// Always use broadcast MAC for DHCP replies
	dstMAC := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

	udpLen := 8 + len(dhcpData)
	udp := make([]byte, udpLen)
	binary.BigEndian.PutUint16(udp[0:], 67)    // src port
	binary.BigEndian.PutUint16(udp[2:], 68)    // dst port
	binary.BigEndian.PutUint16(udp[4:], uint16(udpLen))
	binary.BigEndian.PutUint16(udp[6:], 0)     // checksum placeholder
	copy(udp[8:], dhcpData)

	// Compute UDP checksum with pseudo-header
	cksum := udpChecksum(srcIP, dstIP, udp)
	binary.BigEndian.PutUint16(udp[6:], cksum)

	// Build IP header (20 bytes, no options)
	ipLen := 20 + len(udp)
	ip := make([]byte, ipLen)
	ip[0] = 0x45                      // version=4, IHL=5
	ip[1] = 0                         // TOS
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen))
	binary.BigEndian.PutUint16(ip[4:], 0) // identification
	ip[6] = 0                         // flags=0 (no DF), fragment offset=0
	ip[7] = 0
	ip[8] = 255                       // TTL
	ip[9] = 17                        // protocol = UDP
	binary.BigEndian.PutUint16(ip[10:], 0) // checksum placeholder
	copy(ip[12:16], srcIP)
	copy(ip[16:20], dstIP)
	copy(ip[20:], udp) // Copy UDP data into IP payload

	// Compute IP header checksum (only over the 20-byte IP header, not the payload)
	ipCK := ipChecksum(ip[:20])
	binary.BigEndian.PutUint16(ip[10:], ipCK)

	// Build Ethernet frame: eth(14) + ip(20+len(udp))
	frame := make([]byte, 14+len(ip))
	copy(frame[14:], ip)

	// Destination MAC: unicast to client or broadcast based on client's broadcast flag
	copy(frame[0:6], dstMAC)
	// Source MAC: our interface MAC
	copy(frame[6:12], s.ifHWAddr)
	// EtherType: IPv4
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)

	// Send via AF_PACKET
	dst := &syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_IP),
		Ifindex:  s.ifIndex,
		Halen:    6,
	}
	copy(dst.Addr[:6], dstMAC)

	if err := syscall.Sendto(s.rawFD, frame, 0, dst); err != nil {
		util.LogWarn("dhcp: send %s to %v: %v", msgTypeName(msgType), dstIP, err)
	} else {
		hex := ""
		for i := 0; i < len(frame) && i < 42; i++ {
			hex += fmt.Sprintf("%02x", frame[i])
		}
		util.LogInfo("dhcp: sent %d bytes via AF_PACKET fd=%d frame_hex=%s", len(frame), s.rawFD, hex)
	}
}

func msgTypeName(t byte) string {
	switch t {
	case dhcpDiscover:
		return "DISCOVER"
	case dhcpOffer:
		return "OFFER"
	case dhcpRequest:
		return "REQUEST"
	case dhcpACK:
		return "ACK"
	case dhcpNAK:
		return "NAK"
	default:
		return fmt.Sprintf("type=%d", t)
	}
}

func dhcpIPToUint32(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip4)
}

func dhcpUint32ToIP(n uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, n)
	return ip
}

func htons(v uint16) uint16 {
	return (v<<8)&0xff00 | (v>>8)&0x00ff
}

// ipChecksum computes the IP header checksum (RFC 1071).
func ipChecksum(data []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(data[i:]))
	}
	if len(data)%2 != 0 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// udpChecksum computes the UDP checksum including a IPv4 pseudo-header.
func udpChecksum(src, dst net.IP, udpPacket []byte) uint16 {
	var sum uint32
	// Pseudo-header
	sum += uint32(src[0])<<8 | uint32(src[1])
	sum += uint32(src[2])<<8 | uint32(src[3])
	sum += uint32(dst[0])<<8 | uint32(dst[1])
	sum += uint32(dst[2])<<8 | uint32(dst[3])
	sum += 17 // UDP protocol
	sum += uint32(len(udpPacket))
	// UDP data
	for i := 0; i+1 < len(udpPacket); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(udpPacket[i:]))
	}
	if len(udpPacket)%2 != 0 {
		sum += uint32(udpPacket[len(udpPacket)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	cksum := ^uint16(sum)
	if cksum == 0 {
		cksum = 0xffff // RFC 768: zero checksum value means "no checksum"
	}
	return cksum
}

func (s *dhcpServerImpl) UpdateStaticBindings(bindings []config.DHCPStaticBinding) {
	newBindings := make(map[string]net.IP)
	for _, b := range bindings {
		mac := normalizeMAC(b.MAC)
		if ip := net.ParseIP(b.IP); ip != nil && mac != "" {
			newBindings[mac] = ip.To4()
		}
	}
	s.mu.Lock()
	s.staticBindings = newBindings
	s.mu.Unlock()
}

func normalizeMAC(s string) string {
	hw, err := net.ParseMAC(s)
	if err != nil {
		return ""
	}
	return hw.String()
}
