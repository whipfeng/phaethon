package connlog

import (
	"fmt"
	"sync"
	"time"

	"phaethon/util"
)

const maxLogs = 100
const notifyDebounce = 100 * time.Millisecond

type Event struct {
	Seq       uint64    `json:"seq"`
	Time      time.Time `json:"time"`
	Inbound   string    `json:"inbound"`
	Protocol  string    `json:"protocol"`
	SrcAddr   string    `json:"srcAddr,omitempty"`
	DstAddr   string    `json:"dstAddr"`
	DstPort   int       `json:"dstPort"`
	Proxy     string    `json:"proxy"`
	Status    string    `json:"status"`
	Error     string    `json:"error,omitempty"`
}

var (
	mu           sync.RWMutex
	logs         []Event
	nextSeq      uint64 = 1
	version      uint64
	notifyTimer  *time.Timer
	notifyMu     sync.Mutex
)

func Log(inbound, protocol, srcAddr, dstAddr string, dstPort int, proxy, status string, err error) {
	e := Event{
		Time:     time.Now(),
		Inbound:  inbound,
		Protocol: protocol,
		SrcAddr:  srcAddr,
		DstAddr:  dstAddr,
		DstPort:  dstPort,
		Proxy:    proxy,
		Status:   status,
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
	mu.Unlock()

	scheduleNotify()
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
	if proxy == "" {
		proxy = "DIRECT"
	}
	if e.Error != "" {
		return fmt.Sprintf("%s %s %s:%d → %s (%s)", icon, e.Protocol, e.DstAddr, e.DstPort, proxy, e.Error)
	}
	return fmt.Sprintf("%s %s %s:%d → %s", icon, e.Protocol, e.DstAddr, e.DstPort, proxy)
}
