package connlog

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/db"
)

const maxLogs = 100
const notifyDebounce = 3 * time.Second

type Event struct {
	Seq             uint64    `json:"seq"`
	Time            time.Time `json:"time"`
	Inbound         string    `json:"inbound"`
	Protocol        string    `json:"protocol"`
	SrcAddr         string    `json:"srcAddr,omitempty"`
	SrcPort         int       `json:"srcPort,omitempty"`         // client source port (when known)
	OriginalDstAddr string    `json:"originalDstAddr,omitempty"` // Original destination before resolver redirection
	DstAddr         string    `json:"dstAddr"`
	DstPort         int       `json:"dstPort"`
	Proxy           string    `json:"proxy"`
	ActualProxy     string    `json:"actualProxy,omitempty"` // Actual proxy used (after group resolution)
	Mapping         string    `json:"mapping,omitempty"`
	Rule            string    `json:"rule,omitempty"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
	TimeRange       string    `json:"timeRange,omitempty"`
	ConnID          string    `json:"connId,omitempty"` // links the event to an active-conn entry
	Via             string    `json:"via,omitempty"`    // mesh egress node ID when egress is the mesh network
	FwdKey          string    `json:"fwdKey,omitempty"` // forwarding 5-tuple key, stable across mesh hops (D-group correlation)
}

// FwdKey builds the cross-node correlation key for a forwarded flow. It must
// use the POST-SNAT forwarding view (VIP source) so every mesh hop computes
// the identical key for the same flow.
func FwdKey(srcAddr string, srcPort int, dstAddr string, dstPort int) string {
	return fmt.Sprintf("%s:%d>%s:%d", srcAddr, srcPort, dstAddr, dstPort)
}

// LogOption annotates an Event with fields the plain Log signature cannot
// carry without breaking every direct caller.
type LogOption func(*Event)

func WithConnID(id string) LogOption {
	return func(e *Event) { e.ConnID = id }
}

func WithSrcPort(p int) LogOption {
	return func(e *Event) { e.SrcPort = p }
}

func WithVia(v string) LogOption {
	return func(e *Event) { e.Via = v }
}

// WithFwdKey sets the cross-node correlation key explicitly. MeshDial
// supplies the inner 5-tuple (VIP source, fakeIP target) — identical on
// every hop — overriding the default display-view key built in Log.
func WithFwdKey(key string) LogOption {
	return func(e *Event) { e.FwdKey = key }
}

var (
	mu               sync.RWMutex
	logs             []Event
	nextSeq          uint64 = 1
	version          atomic.Uint64
	totalConnections uint64
	notifyTimer      *time.Timer
	notifyMu         sync.Mutex
)

func Log(inbound, protocol, srcAddr, originalDstAddr, dstAddr string, dstPort int, matchResult *config.MatchResult, status string, err error, opts ...LogOption) {
	e := Event{
		Time:            time.Now(),
		Inbound:         inbound,
		Protocol:        protocol,
		SrcAddr:         srcAddr,
		OriginalDstAddr: originalDstAddr,
		DstAddr:         dstAddr,
		DstPort:         dstPort,
		Status:          status,
	}
	if matchResult != nil {
		e.Proxy = matchResult.ProxyName
		e.ActualProxy = matchResult.ActualProxy
		e.Mapping = matchResult.Mapping
		e.TimeRange = matchResult.TimeRange
		e.Rule = matchResult.Rule
	}
	if err != nil {
		e.Error = err.Error()
	}
	for _, opt := range opts {
		opt(&e)
	}
	// FwdKey defaults to the recorded 5-tuple (display view); WithFwdSrc
	// replaces it with the post-SNAT forwarding view. Skipped when no source
	// port is known (e.g. TUN SYSTEM events) — the key would be meaningless.
	if e.FwdKey == "" && e.SrcPort > 0 {
		e.FwdKey = FwdKey(srcAddr, e.SrcPort, dstAddr, dstPort)
	}

	mu.Lock()
	e.Seq = nextSeq
	nextSeq++
	logs = append(logs, e)
	if len(logs) > maxLogs {
		logs = logs[len(logs)-maxLogs:]
	}
	version.Add(1)
	totalConnections++
	mu.Unlock()

	scheduleNotify()
}

func GetTotalConnections() uint64 {
	mu.RLock()
	defer mu.RUnlock()
	return totalConnections
}

func scheduleNotify() {
	notifyMu.Lock()
	defer notifyMu.Unlock()

	if notifyTimer != nil {
		return
	}

	notifyTimer = time.AfterFunc(notifyDebounce, func() {
		notifyMu.Lock()
		notifyTimer = nil
		notifyMu.Unlock()

		db.BumpRuntimeVersion("logs")
	})
}

func GetLogs() []Event {
	mu.RLock()
	defer mu.RUnlock()
	result := make([]Event, len(logs))
	copy(result, logs)
	return result
}

func GetLogsAfterSeq(seq uint64) (result []Event, stale bool) {
	mu.RLock()
	defer mu.RUnlock()
	if seq > version.Load() {
		// Cursor from before a process restart (version counter reset):
		// seqs no longer reach it, so return a full snapshot and let the
		// client resync instead of freezing on an empty incremental.
		out := make([]Event, len(logs))
		copy(out, logs)
		return out, true
	}
	for _, e := range logs {
		if e.Seq > seq {
			result = append(result, e)
		}
	}
	return result, false
}

func GetVersion() uint64 {
	return version.Load()
}
