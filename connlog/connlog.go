package connlog

import (
	"fmt"
	"sync"
	"time"

	"phaethon/config"
	"phaethon/util"
)

const maxLogs = 100
const notifyDebounce = 3 * time.Second

type Event struct {
	Seq            uint64    `json:"seq"`
	Time           time.Time `json:"time"`
	Inbound        string    `json:"inbound"`
	Protocol       string    `json:"protocol"`
	SrcAddr        string    `json:"srcAddr,omitempty"`
	OriginalDstAddr string   `json:"originalDstAddr,omitempty"` // Original destination before resolver redirection
	DstAddr        string    `json:"dstAddr"`
	DstPort        int       `json:"dstPort"`
	Proxy          string    `json:"proxy"`
	ActualProxy    string    `json:"actualProxy,omitempty"` // Actual proxy used (after group resolution)
	Mapping        string    `json:"mapping,omitempty"`
	Rule           string    `json:"rule,omitempty"`
	Status         string    `json:"status"`
	Error          string    `json:"error,omitempty"`
	TimeRange      string    `json:"timeRange,omitempty"`
}

var (
	mu              sync.RWMutex
	logs            []Event
	nextSeq         uint64 = 1
	version         uint64
	totalConnections uint64
	notifyTimer     *time.Timer
	notifyMu        sync.Mutex
)

func Log(inbound, protocol, srcAddr, originalDstAddr, dstAddr string, dstPort int, matchResult *config.MatchResult, status string, err error) {
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

	mu.Lock()
	e.Seq = nextSeq
	nextSeq++
	logs = append(logs, e)
	if len(logs) > maxLogs {
		logs = logs[len(logs)-maxLogs:]
	}
	version++
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

		util.DefaultVersionNotifier.BumpVersion("logs")
	})
}

func GetLogs() []Event {
	mu.RLock()
	defer mu.RUnlock()
	result := make([]Event, len(logs))
	copy(result, logs)
	return result
}

func GetLogsAfterSeq(seq uint64) []Event {
	mu.RLock()
	defer mu.RUnlock()
	var result []Event
	for _, e := range logs {
		if e.Seq > seq {
			result = append(result, e)
		}
	}
	return result
}

func GetVersion() uint64 {
	mu.RLock()
	defer mu.RUnlock()
	return version
}

func FormatEvent(e Event) string {
	icon := "✓"
	if e.Status == "fail" || e.Status == "reject" {
		icon = "✗"
	}
	proxy := e.Proxy
	if proxy == "" && e.Status == "ok" {
		proxy = "DIRECT"
	}
	
	// Show original -> resolved when they differ
	dstDisplay := fmt.Sprintf("%s:%d", e.DstAddr, e.DstPort)
	if e.OriginalDstAddr != "" && e.OriginalDstAddr != e.DstAddr {
		dstDisplay = fmt.Sprintf("%s → %s:%d", e.OriginalDstAddr, e.DstAddr, e.DstPort)
	}
	
	if e.Error != "" {
		return fmt.Sprintf("%s %s %s → %s (%s)", icon, e.Protocol, dstDisplay, proxy, e.Error)
	}
	return fmt.Sprintf("%s %s %s → %s", icon, e.Protocol, dstDisplay, proxy)
}
