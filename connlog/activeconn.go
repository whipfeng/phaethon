package connlog

import (
	"sort"
	"sync"
	"time"

	"phaethon/config"
)

const maxActive = 500
const maxJournal = 500

type ActiveConn struct {
	ID              string    `json:"id"`
	Protocol        string    `json:"protocol"`
	Inbound         string    `json:"inbound"`
	SrcAddr         string    `json:"srcAddr,omitempty"`
	OriginalDstAddr string    `json:"originalDstAddr,omitempty"`
	DstAddr         string    `json:"dstAddr"`
	DstPort         int       `json:"dstPort"`
	Proxy           string    `json:"proxy"`
	ActualProxy     string    `json:"actualProxy,omitempty"`
	Mapping         string    `json:"mapping,omitempty"`
	Rule            string    `json:"rule,omitempty"`
	TimeRange       string    `json:"timeRange,omitempty"`
	StartTime       time.Time `json:"startTime"`
}

type JournalEntry struct {
	Seq    uint64      `json:"seq"`
	Action string      `json:"action"`
	Conn   *ActiveConn `json:"conn,omitempty"`
	ID     string      `json:"id,omitempty"`
}

var (
	activeMu  sync.RWMutex
	activeMap = make(map[string]*ActiveConn)
	journal   []JournalEntry
)

func TrackActive(id, inbound, protocol, srcAddr, originalDstAddr, dstAddr string, dstPort int, matchResult *config.MatchResult) {
	conn := &ActiveConn{
		ID:              id,
		Protocol:        protocol,
		Inbound:         inbound,
		SrcAddr:         srcAddr,
		OriginalDstAddr: originalDstAddr,
		DstAddr:         dstAddr,
		DstPort:         dstPort,
		StartTime:       time.Now(),
	}
	if matchResult != nil {
		conn.Proxy = matchResult.ProxyName
		conn.ActualProxy = matchResult.ActualProxy
		conn.Mapping = matchResult.Mapping
		conn.TimeRange = matchResult.TimeRange
		conn.Rule = matchResult.Rule
	}

	activeMu.Lock()
	if len(activeMap) >= maxActive {
		activeMu.Unlock()
		return
	}
	activeMap[id] = conn

	mu.Lock()
	version++
	entry := JournalEntry{
		Seq:    version,
		Action: "add",
		Conn:   conn,
	}
	journal = append(journal, entry)
	if len(journal) > maxJournal {
		journal = journal[len(journal)-maxJournal:]
	}
	mu.Unlock()
	activeMu.Unlock()

	scheduleNotify()
}

func RemoveActive(id string) {
	activeMu.Lock()
	if _, ok := activeMap[id]; !ok {
		activeMu.Unlock()
		return
	}
	delete(activeMap, id)

	mu.Lock()
	version++
	entry := JournalEntry{
		Seq:    version,
		Action: "remove",
		ID:     id,
	}
	journal = append(journal, entry)
	if len(journal) > maxJournal {
		journal = journal[len(journal)-maxJournal:]
	}
	mu.Unlock()
	activeMu.Unlock()

	scheduleNotify()
}

func GetActiveConns() []ActiveConn {
	activeMu.RLock()
	defer activeMu.RUnlock()
	result := make([]ActiveConn, 0, len(activeMap))
	for _, c := range activeMap {
		result = append(result, *c)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].StartTime.Before(result[j].StartTime)
	})
	return result
}

func GetActiveConnsAfterSeq(seq uint64) (entries []JournalEntry, conns []ActiveConn, stale bool) {
	activeMu.RLock()
	defer activeMu.RUnlock()

	if len(journal) == 0 || seq < journal[0].Seq {
		conns = make([]ActiveConn, 0, len(activeMap))
		for _, c := range activeMap {
			conns = append(conns, *c)
		}
		sort.Slice(conns, func(i, j int) bool {
			return conns[i].StartTime.Before(conns[j].StartTime)
		})
		return nil, conns, true
	}

	for _, e := range journal {
		if e.Seq > seq {
			entries = append(entries, e)
		}
	}
	return entries, nil, false
}

func GetActiveCount() int {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return len(activeMap)
}
