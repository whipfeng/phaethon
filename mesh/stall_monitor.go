package mesh

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

	// Probe implementations, swappable in tests. Defaults exercise the real
	// paths: mesh leg = in-stack socket to a peer GIP (Netstack.ProbeMeshPeerDNS),
	// host leg = host-native socket to the local GIP (probeHostViaOwnGIP).
	probeMeshPeer func(gip net.IP, timeout time.Duration) error
	probeHost     func(gip net.IP, timeout time.Duration) error
	ownGIP        func() net.IP
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
	sm := &StallMonitor{
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
	sm.probeMeshPeer = func(gip net.IP, timeout time.Duration) error {
		if sm.ns == nil {
			return fmt.Errorf("no netstack")
		}
		return sm.ns.ProbeMeshPeerDNS(gip, timeout)
	}
	sm.probeHost = probeHostViaOwnGIP
	sm.ownGIP = func() net.IP {
		if sm.ns == nil {
			return nil
		}
		addr := sm.ns.DNSAddr()
		return net.IP(addr.AsSlice())
	}
	return sm
}

const (
	tickInterval  = 10 * time.Second
	freezeWindow  = 60 * time.Second
	probeTimeout  = 3 * time.Second
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

// --- precise-dump ladder (docs/plans/stall-monitor-precise-dump-ladder.md) ---

type stallCounters struct {
	A     uint64 `json:"a"`
	B     uint64 `json:"b"`
	DVIP  uint64 `json:"dvip"`
	DIPIP uint64 `json:"dipip"`
}

type stallPeerSnap struct {
	Node       string  `json:"node"`
	SRTTMs     float64 `json:"srttMs,omitempty"`
	JitterMs   float64 `json:"jitterMs,omitempty"`
	LossRate   float64 `json:"lossRate,omitempty"`
	ConsecLost int     `json:"consecLost"`
	Samples    int     `json:"samples,omitempty"`
	LastSeenMs int64   `json:"lastSeenAgeMs"`
}

type stallLinkNICSnap struct {
	Peer   string `json:"peer"`
	Inject uint64 `json:"injectPackets"`
	Drops  uint64 `json:"injectDrops"`
}

type stallSnapshot struct {
	Event       string             `json:"event"`
	Rule        int                `json:"rule"`
	RuleName    string             `json:"ruleName"`
	Counters    stallCounters      `json:"counters"`
	Peers       []stallPeerSnap    `json:"peers"`
	LinkNICs    []stallLinkNICSnap `json:"linkNics,omitempty"`
	Disposition string             `json:"disposition"`
}

type probeOutcome struct {
	Target string `json:"target"`
	Alive  bool   `json:"alive"`
	Err    string `json:"err,omitempty"`
}

// classifyDisposition maps L1 snapshot evidence to a triage verdict.
// Pure function; thresholds are half of pingFailDisconnectThreshold (10) —
// by fire time (freezeWindow 60s after the last target movement, pings at
// 1s cadence) a truly dead link has already accumulated stale pongs.
func classifyDisposition(ruleIdx int, peers []stallPeerSnap) string {
	if len(peers) == 0 {
		return "ambiguous"
	}
	for _, p := range peers {
		if p.ConsecLost >= 5 || p.LossRate > 0.3 {
			return "dead-link-suspect"
		}
	}
	if ruleIdx == 2 {
		return "host-silent-suspect"
	}
	return "ping-healthy"
}

func (sm *StallMonitor) buildSnapshot(ruleIdx int, s sample) stallSnapshot {
	name := "manual"
	if ruleIdx >= 0 && ruleIdx < len(stallRules) {
		name = stallRules[ruleIdx].name
	}
	peers := sm.collectPeers()
	return stallSnapshot{
		Event:       "stall-snapshot",
		Rule:        ruleIdx,
		RuleName:    name,
		Counters:    stallCounters{A: s.a, B: s.b, DVIP: s.dVIP, DIPIP: s.dIPIP},
		Peers:       peers,
		LinkNICs:    sm.collectLinkNICs(),
		Disposition: classifyDisposition(ruleIdx, peers),
	}
}

func (sm *StallMonitor) collectPeers() []stallPeerSnap {
	infos := sm.m.GetPeers()
	snaps := make([]stallPeerSnap, 0, len(infos))
	for _, info := range infos {
		snaps = append(snaps, stallPeerSnap{
			Node:       info.NodeID,
			SRTTMs:     info.SRTT,
			JitterMs:   info.Jitter,
			LossRate:   info.LossRate,
			ConsecLost: sm.m.GetLinkConsecutiveLost(info.NodeID, info.LocalSeq),
			Samples:    info.SampleCount,
			LastSeenMs: time.Since(info.LastSeen).Milliseconds(),
		})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Node < snaps[j].Node })
	return snaps
}

func (sm *StallMonitor) collectLinkNICs() []stallLinkNICSnap {
	stats := sm.m.GetLinkNICStats()
	snaps := make([]stallLinkNICSnap, 0, len(stats))
	for peer, st := range stats {
		snaps = append(snaps, stallLinkNICSnap{Peer: peer, Inject: st.Packets, Drops: st.Drops})
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Peer < snaps[j].Peer })
	return snaps
}

// meshProbeTargets returns the GIP of every connected peer (derived from its
// subnet, .3 = DNS). Peers without a parseable subnet are skipped.
func (sm *StallMonitor) meshProbeTargets() []net.IP {
	infos := sm.m.GetPeers()
	targets := make([]net.IP, 0, len(infos))
	for _, info := range infos {
		_, sn, err := net.ParseCIDR(info.Subnet)
		if err != nil {
			continue
		}
		if gip := DeriveGIPFromSubnet(sn); gip != nil {
			targets = append(targets, gip)
		}
	}
	return targets
}

// runProbes executes the L2 legs. Mesh leg: in-stack socket to every peer
// GIP (always — the results grade the dump). Host leg (rule 2 only): a
// host-native socket to the local GIP, which crosses the TUN twice and so
// proves the whole host↔TUN↔netstack loop alive. Legs run in parallel,
// take no business locks, and only their outcomes are returned.
func (sm *StallMonitor) runProbes(ruleIdx int) (mesh []probeOutcome, host probeOutcome) {
	var wg sync.WaitGroup
	host = probeOutcome{Target: "skipped(not-rule-2)"}

	targets := sm.meshProbeTargets()
	mesh = make([]probeOutcome, len(targets))
	for i, gip := range targets {
		wg.Add(1)
		go func(i int, gip net.IP) {
			defer wg.Done()
			err := sm.probeMeshPeer(gip, probeTimeout)
			out := probeOutcome{Target: gip.String(), Alive: err == nil}
			if err != nil {
				out.Err = err.Error()
			}
			mesh[i] = out
		}(i, gip)
	}

	// Host leg on rule 2 (host-silent triage) and on -1 (RunDiagnostics
	// self-check verifies both legs after deploy).
	if ruleIdx == 2 || ruleIdx == -1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gip := sm.ownGIP()
			if gip == nil {
				host = probeOutcome{Target: "gip", Err: "no netstack"}
				return
			}
			err := sm.probeHost(gip, probeTimeout)
			host = probeOutcome{Target: gip.String(), Alive: err == nil}
			if err != nil {
				host.Err = err.Error()
			}
		}()
	}

	wg.Wait()
	return mesh, host
}

// probeHostViaOwnGIP is the host leg: a host-native UDP DNS query to the
// local GIP. From the host side this crosses the TUN twice — host→wintun
// (the counter-A read path), netstack reply→wintun (the counter-B write
// path) — so any reply proves the whole loop alive. That is the rule 2
// "host-silent" verdict: nothing on the host was producing TUN traffic,
// which is benign. A timeout means the loop is broken somewhere and a dump
// is warranted. No listener, firewall rule, or port config is assumed: the
// DNS hijacker on GIP:53 is an architectural invariant.
func probeHostViaOwnGIP(gip net.IP, timeout time.Duration) error {
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: gip, Port: 53})
	if err != nil {
		return fmt.Errorf("probe host: dial: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("probe host: deadline: %v", err)
	}
	query := BuildDNSQuery("probe.phn", uint16(time.Now().UnixNano()))
	if _, err := conn.Write(query); err != nil {
		return fmt.Errorf("probe host: write: %v", err)
	}
	buf := make([]byte, 512)
	if _, err := conn.Read(buf); err != nil {
		return fmt.Errorf("probe host: read: %v", err)
	}
	return nil
}

func (sm *StallMonitor) fire(ruleIdx int, s sample, now time.Time) {
	rule := stallRules[ruleIdx]
	snap := sm.buildSnapshot(ruleIdx, s)
	snapLine, _ := json.Marshal(snap)
	util.LogModuleWarn("mesh", "[STALL] %s", snapLine)

	mesh, host := sm.runProbes(ruleIdx)
	probeLine, _ := json.Marshal(map[string]interface{}{
		"event":      "stall-probes",
		"meshProbes": mesh,
		"hostProbe":  host,
	})
	util.LogModuleWarn("mesh", "[STALL] %s", probeLine)

	// rule 2's only benign explanation: the host loop answered, so the frozen
	// A counter means host-silent, not a dead read path. Every other
	// signature keeps the dump (probe results grade the evidence).
	if ruleIdx == 2 && host.Alive {
		util.LogModuleWarn("mesh", "[STALL] rule %s: host loop alive (host-silent), dump skipped", rule.name)
		return
	}

	path, err := sm.writeDump("auto:"+rule.name, string(snapLine)+"\n"+string(probeLine))
	if err != nil {
		util.LogModuleError("mesh", "[STALL] dump write failed: %v", err)
		return
	}
	util.LogModuleWarn("mesh", "[STALL] dump written: %s", path)
}

// RunDiagnostics runs the L1 snapshot + L2 probes on demand (admin
// self-check after deploy). Returns combined JSON; never writes a dump.
func (sm *StallMonitor) RunDiagnostics() ([]byte, error) {
	s := sm.collect(time.Now())
	snap := sm.buildSnapshot(-1, s)
	mesh, host := sm.runProbes(-1)
	return json.Marshal(map[string]interface{}{
		"snapshot":   snap,
		"meshProbes": mesh,
		"hostProbe":  host,
	})
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
	if sm.ns != nil {
		w("netstack-read=%d nic1-backlog=%d uturn=%d",
			sm.ns.ReadPackets.Load(), sm.ns.LinkEP().NumQueued(), sm.m.uturnCount)
	} else {
		w("netstack-read=unavailable nic1-backlog=unavailable uturn=%d", sm.m.uturnCount)
	}
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

	if sm.ns != nil {
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
