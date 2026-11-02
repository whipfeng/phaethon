package mesh

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"phaethon/util"
)

// StallMonitor watches three causal promises of the local data plane and
// captures a full diagnostic dump when one breaks:
//
//	rule 0 (A↑ D frozen ≥60s): outbound without returns — blackhole upstream
//	rule 1 (D↑ B frozen ≥60s): host-facing frame arrived but never written to TUN
//	rule 2 (B↑ A frozen ≥60s): TUN writes flowing but host produces nothing —
//	    read-loop dead OR host silent; resolved by an active liveness probe
//
// Eligibility gate: a rule only fires if its TARGET counter was meaningfully
// active during the recent-samples window (≥ minTargetActivity frames over
// ~120s) before going silent. A counter that has been sparse all along (e.g.
// D on a client-role node, which only counts peer-initiated frames to the
// local VIP) can freeze for minutes as a matter of course — "frozen only
// matters if it was recently flowing". Without this gate, rule 0 fired a
// dump every few minutes on client-role nodes during daytime traffic
// (2026-11-02: 5 dumps in 40 min, all false positives, zero stuck frames).
//
// Counters: A = TUN readPackets (tun manager), B = netstack WritePackets,
// D = mesh-inbound frames destined to local VIP or IPIP-encapsulated to local
// EIP (see HandleMeshFrame). The monitor only sleeps, loads atomics and
// occasionally writes a file — it never takes business locks, so it keeps
// observing while the data plane itself is dead. See
// docs/tasks/mesh-data-plane-stall-tasks.md (路径2) for the full design,
// recorded assumptions, and known limits.
type StallMonitor struct {
	m       *MeshManager
	ns      *Netstack
	readA   func() uint64
	version string
	logsDir string
	started time.Time

	stopCh <-chan struct{}

	dumpMu sync.Mutex

	// rule evaluation state
	initialized bool          // first sample establishes the baseline for all counters
	lastChange  [3]time.Time // index 0=A, 1=B, 2=D
	prev        [3]uint64
	armed       [3]bool
	firedValue  [3]uint64 // counter value at fire; re-arm when it moves again

	// recent samples for dump context (ring buffer)
	samples    [12]sample
	sampleIdx  int
	sampleFill int

	startOnce sync.Once
	startedCh chan struct{}
}

type sample struct {
	t    time.Time
	a, b uint64
	dVIP uint64
	dIPIP uint64
}

// NewStallMonitor creates the monitor. readA must return the TUN readPackets
// counter (wired from the tun manager by main to avoid an import cycle).
func NewStallMonitor(m *MeshManager, ns *Netstack, readA func() uint64, version, logsDir string) *StallMonitor {
	return &StallMonitor{
		m:         m,
		ns:        ns,
		readA:     readA,
		version:   version,
		logsDir:   logsDir,
		started:   time.Now(),
		stopCh:    m.CloseCh(),
		armed:     [3]bool{true, true, true},
		startedCh: make(chan struct{}),
	}
}

const (
	tickInterval  = 10 * time.Second
	freezeWindow  = 60 * time.Second
	probeWait     = 5 * time.Second
	probeSrcPort  = 40555
	probeDstPort  = 9 // discard; closed on any host → RST
	dumpMaxBuffer = 64 << 20

	// minTargetActivity is the eligibility floor for rule targets: the target
	// counter must have moved by at least this many frames across the recent-
	// samples window (~120s) for a freeze to be actionable. 60 frames/120s
	// (0.5/s sustained) separates "was flowing and stopped" from "never flows
	// on this node's role".
	minTargetActivity = 60
)

// Start launches the background goroutine. Idempotent.
func (sm *StallMonitor) Start() {
	sm.startOnce.Do(func() {
		close(sm.startedCh)
		go sm.loop()
	})
}

func (sm *StallMonitor) loop() {
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-sm.stopCh:
			return
		case now := <-ticker.C:
			s := sm.collect(now)
			sm.recordSample(s)
			for _, rule := range sm.evaluate(s, now) {
				sm.fire(rule, s, now)
			}
		}
	}
}

func (sm *StallMonitor) collect(t time.Time) sample {
	var s sample
	s.t = t
	if sm.readA != nil {
		s.a = sm.readA()
	}
	if sm.ns != nil {
		s.b = sm.ns.WritePackets.Load()
	}
	s.dVIP = sm.m.stallDVIP.Load()
	s.dIPIP = sm.m.stallDIPIP.Load()
	return s
}

func (sm *StallMonitor) dTotal(s sample) uint64 { return s.dVIP + s.dIPIP }

// sampleValue extracts a rule-indexed counter (0=A, 1=B, 2=D) from a sample.
func sampleValue(idx int, s sample) uint64 {
	switch idx {
	case 0:
		return s.a
	case 1:
		return s.b
	default:
		return s.dVIP + s.dIPIP
	}
}

// windowDelta returns how much the given counter moved between the oldest and
// newest samples currently in the ring. Callers must ensure recordSample(s)
// ran for the current sample before evaluate (the loop does; tests must too).
func (sm *StallMonitor) windowDelta(idx int) uint64 {
	if sm.sampleFill < 2 {
		return 0
	}
	oldest := sm.samples[(sm.sampleIdx-sm.sampleFill+len(sm.samples))%len(sm.samples)]
	newest := sm.samples[(sm.sampleIdx-1+len(sm.samples))%len(sm.samples)]
	return sampleValue(idx, newest) - sampleValue(idx, oldest)
}

func (sm *StallMonitor) recordSample(s sample) {
	sm.samples[sm.sampleIdx] = s
	sm.sampleIdx = (sm.sampleIdx + 1) % len(sm.samples)
	if sm.sampleFill < len(sm.samples) {
		sm.sampleFill++
	}
}

// rules pairs premise and target counter indexes.
// idx: 0=A, 1=B, 2=D
var stallRules = []struct {
	premise int
	target  int
	name    string
}{
	{0, 2, "A-rising-D-frozen(outbound-blackhole)"},
	{2, 1, "D-rising-B-frozen(delivery-stall)"},
	{1, 0, "B-rising-A-frozen(readloop-or-host-silent)"},
}

func (sm *StallMonitor) values(s sample) [3]uint64 {
	return [3]uint64{s.a, s.b, sm.dTotal(s)}
}

// evaluate updates last-change tracking and returns the rules that fired.
// Kept side-effect-free apart from timestamp/state updates so it is unit
// testable: feed samples with increasing t.
func (sm *StallMonitor) evaluate(s sample, now time.Time) []int {
	v := sm.values(s)
	if !sm.initialized {
		// First sample is the baseline: without it zero-value timestamps would
		// make every rule's "frozen" test pass immediately.
		for i := range v {
			sm.lastChange[i] = now
			sm.prev[i] = v[i]
		}
		sm.initialized = true
		return nil
	}
	var fired []int
	for i := range v {
		if v[i] != sm.prev[i] {
			sm.lastChange[i] = now
			sm.prev[i] = v[i]
		}
	}
	for ruleIdx, r := range stallRules {
		if !sm.armed[r.target] {
			// re-arm once the target counter moves again (episode recovered)
			if v[r.target] != sm.firedValue[r.target] {
				sm.armed[r.target] = true
			}
		}
		if !sm.armed[r.target] {
			continue
		}
		premiseRecent := now.Sub(sm.lastChange[r.premise]) <= freezeWindow
		premiseAfterTarget := sm.lastChange[r.premise].After(sm.lastChange[r.target])
		targetFrozen := now.Sub(sm.lastChange[r.target]) >= freezeWindow
		// Eligibility: the target must have been flowing at a meaningful rate
		// across the recent-samples window. Counters that are sparse by role
		// (D on client nodes) freeze constantly as a steady state — not a stall.
		if sm.windowDelta(r.target) < minTargetActivity {
			continue
		}
		if premiseRecent && premiseAfterTarget && targetFrozen {
			sm.armed[r.target] = false
			sm.firedValue[r.target] = v[r.target]
			fired = append(fired, ruleIdx)
		}
	}
	return fired
}

func (sm *StallMonitor) fire(ruleIdx int, s sample, now time.Time) {
	rule := stallRules[ruleIdx]
	util.LogModuleWarn("mesh", "[STALL] rule %s fired at %s (A=%d B=%d DVIP=%d DIPIP=%d), capturing dump",
		rule.name, now.Format(time.RFC3339), s.a, s.b, s.dVIP, s.dIPIP)

	probeResult := ""
	if ruleIdx == 2 {
		probeResult = sm.probeHostLiveness(s)
	}

	path, err := sm.writeDump("auto:"+rule.name, probeResult)
	if err != nil {
		util.LogModuleError("mesh", "[STALL] dump write failed: %v", err)
		return
	}
	util.LogModuleWarn("mesh", "[STALL] dump written: %s", path)
}

// probeHostLiveness distinguishes a dead read loop from a silent host: write a
// TCP SYN addressed to the host's own TUN adapter IP (src = some mesh peer
// VIP). A live host stack must answer RST/SYN-ACK, and the reply routes back
// via the TUN (/1), so a healthy read loop increments A within probeWait.
func (sm *StallMonitor) probeHostLiveness(s sample) string {
	dev := sm.ns.WriteLoopDevice
	if dev == nil {
		return "probe=skipped(no-tun-device)"
	}
	hostIP := sm.m.getHostIP()
	srcIP := sm.pickProbeSrcVIP()
	if hostIP == nil || srcIP == nil {
		return "probe=skipped(no-host-or-peer-ip)"
	}
	pkt := buildSYN(srcIP, hostIP, probeSrcPort, probeDstPort)
	if _, err := dev.Write(pkt); err != nil {
		return fmt.Sprintf("probe=write-error(%v)", err)
	}
	deadline := time.Now().Add(probeWait)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if sm.readA != nil && sm.readA() > s.a {
			return "probe=responded(read-loop-alive)"
		}
		select {
		case <-sm.stopCh:
			return "probe=aborted(shutdown)"
		default:
		}
	}
	return "probe=timeout(read-loop-or-host-stack-dead)"
}

// pickProbeSrcVIP returns any peer's VIP so the host's RST routes back via
// the TUN (/1 covers the mesh range). Any mesh-routable address works; a real
// peer VIP is guaranteed to be inside the mesh network.
func (sm *StallMonitor) pickProbeSrcVIP() net.IP {
	rt := sm.m.getRouteTable()
	if rt == nil {
		return nil
	}
	for subnetStr, nodeID := range rt.subnetOwner {
		if nodeID == sm.m.nodeID {
			continue
		}
		if _, sn, err := net.ParseCIDR(subnetStr); err == nil {
			return CalculateVIP(sn)
		}
	}
	return nil
}

// TriggerDump captures a dump immediately (manual / on-demand path).
func (sm *StallMonitor) TriggerDump(reason string) (string, error) {
	return sm.writeDump("manual:"+reason, "")
}

func (sm *StallMonitor) writeDump(trigger, probeResult string) (string, error) {
	sm.dumpMu.Lock()
	defer sm.dumpMu.Unlock()

	if err := os.MkdirAll(sm.logsDir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(sm.logsDir, "goroutine-dump-"+time.Now().Format("20060102-150405")+".log")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	s := sm.collect(time.Now())
	w := func(format string, args ...interface{}) { fmt.Fprintf(f, format+"\n", args...) }

	w("=== phaethon stall dump ===")
	w("time: %s", s.t.Format(time.RFC3339))
	w("version: %s", sm.version)
	w("go: %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	w("uptime: %s", time.Since(sm.started).Round(time.Second))
	w("node-id: %s", sm.m.GetNodeID())
	w("subnet: %s vip: %s host-ip: %s", sm.m.GetSubnet(), sm.m.GetVIP(), sm.m.getHostIP())
	w("trigger: %s", trigger)
	if probeResult != "" {
		w("%s", probeResult)
	}
	w("")

	w("--- counters ---")
	w("A(tun-read)=%d B(netstack-write)=%d D-vip=%d D-ipip-eip=%d", s.a, s.b, s.dVIP, s.dIPIP)
	w("netstack-read=%d nic1-backlog=%d uturn=%d",
		sm.ns.ReadPackets.Load(), sm.ns.LinkEP().NumQueued(), sm.m.uturnCount)
	w("")

	w("--- samples (last %d, oldest first) ---", sm.sampleFill)
	for i := 0; i < sm.sampleFill; i++ {
		h := sm.samples[(sm.sampleIdx-sm.sampleFill+i+len(sm.samples)*2)%len(sm.samples)]
		w("%s A=%d B=%d Dvip=%d Dipip=%d", h.t.Format("15:04:05"), h.a, h.b, h.dVIP, h.dIPIP)
	}
	w("")

	if drops, meshInboundDrops := sm.m.GetChannelDropStats(); len(drops) > 0 || meshInboundDrops > 0 {
		w("--- p2p channel drop stats ---")
		w("mesh-inbound-drops=%d", meshInboundDrops)
		for peer, st := range drops {
			w("peer=%s data-drops=%d control-drops=%d", peer, st.DataDrops, st.ControlChDrops)
		}
		w("")
	}

	if st := sm.ns.Stack(); st != nil {
		stats := st.Stats()
		w("--- gvisor ip stats ---")
		w("%+v", stats.IP)
		w("--- gvisor tcp stats ---")
		w("%+v", stats.TCP)
		w("--- gvisor udp stats ---")
		w("%+v", stats.UDP)
		w("")
	}

	w("--- goroutine stacks (all=true) ---")
	buf := make([]byte, 8<<20)
	n := runtime.Stack(buf, true)
	if n == len(buf) {
		buf = make([]byte, dumpMaxBuffer)
		n = runtime.Stack(buf, true)
		if n == len(buf) {
			w("[warning: stack buffer truncated at %d bytes]", n)
		}
	}
	f.Write(buf[:n])

	return path, nil
}

// buildSYN crafts a bare IPv4/TCP SYN with correct checksums. The host OS
// answers it at stack level (RST to a closed port), so no application or
// peer cooperation is required.
func buildSYN(src, dst net.IP, srcPort, dstPort uint16) []byte {
	b := make([]byte, 40)
	b[0] = 0x45 // IPv4, IHL=5
	binary.BigEndian.PutUint16(b[2:4], uint16(len(b)))
	b[8] = 64 // TTL
	b[9] = 6  // TCP
	copy(b[12:16], src.To4())
	copy(b[16:20], dst.To4())
	binary.BigEndian.PutUint16(b[10:12], ipv4Checksum(b[0:20]))

	binary.BigEndian.PutUint16(b[20:22], srcPort)
	binary.BigEndian.PutUint16(b[22:24], dstPort)
	binary.BigEndian.PutUint32(b[24:28], 0x70686e31) // "phn1", arbitrary ISN
	b[33] = 0x02 // SYN
	binary.BigEndian.PutUint16(b[34:36], 64240)

	sum := pseudoHeaderSum(src.To4(), dst.To4(), 6, 20)
	sum = addBytes(sum, b[20:40])
	binary.BigEndian.PutUint16(b[36:38], finalizeSum(sum))
	return b
}

func ipv4Checksum(b []byte) uint16 {
	return finalizeSum(addBytes(0, b))
}

func pseudoHeaderSum(src, dst []byte, proto byte, tcpLen int) uint32 {
	var sum uint32
	sum = addBytes(sum, src)
	sum = addBytes(sum, dst)
	sum += uint32(proto)
	sum += uint32(tcpLen)
	return sum
}

func addBytes(sum uint32, b []byte) uint32 {
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b[:2]))
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	return sum
}

func finalizeSum(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
