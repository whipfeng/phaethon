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
	SrcPort         int       `json:"srcPort,omitempty"`
	OriginalDstAddr string    `json:"originalDstAddr,omitempty"`
	DstAddr         string    `json:"dstAddr"`
	DstPort         int       `json:"dstPort"`
	Proxy           string    `json:"proxy"`
	ActualProxy     string    `json:"actualProxy,omitempty"`
	Mapping         string    `json:"mapping,omitempty"`
	Rule            string    `json:"rule,omitempty"`
	TimeRange       string    `json:"timeRange,omitempty"`
	Via             string    `json:"via,omitempty"`
	FwdKey          string    `json:"fwdKey,omitempty"`
	StartTime       time.Time `json:"startTime"`
}

// ActiveOption carries the optional correlation fields into TrackActive
// without breaking the positional signature at existing call sites.
type ActiveOption func(*ActiveConn)

func WithActiveSrcPort(p int) ActiveOption {
	return func(c *ActiveConn) { c.SrcPort = p }
}

func WithActiveVia(v string) ActiveOption {
	return func(c *ActiveConn) { c.Via = v }
}

// WithActiveFwdKey pins the cross-node correlation key (mesh flows pass the
// inner 5-tuple from MeshDial; leave empty to derive from the display view).
func WithActiveFwdKey(key string) ActiveOption {
	return func(c *ActiveConn) {
		if key != "" {
			c.FwdKey = key
		}
	}
}

type JournalEntry struct {
	Seq    uint64      `json:"seq"`
	Action string      `json:"action"`
	Conn   *ActiveConn `json:"conn,omitempty"`
	ID     string      `json:"id,omitempty"`
}

var (
	activeMap sync.Map // map[string]*ActiveConn
	journal   []JournalEntry
)

func TrackActive(id, inbound, protocol, srcAddr, originalDstAddr, dstAddr string, dstPort int, matchResult *config.MatchResult, opts ...ActiveOption) {
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
	for _, opt := range opts {
		opt(conn)
	}
	// Default FwdKey from the display view when no explicit forwarding source
	// was supplied (e.g. proxy/direct flows that never enter the mesh).
	if conn.FwdKey == "" && conn.SrcPort > 0 {
		conn.FwdKey = FwdKey(srcAddr, conn.SrcPort, dstAddr, dstPort)
	}

	// Check capacity before storing
	count := 0
	activeMap.Range(func(_, _ any) bool {
		count++
		return count < maxActive
	})
	if count >= maxActive {
		return
	}

	activeMap.Store(id, conn)

	mu.Lock()
	seq := version.Add(1)
	entry := JournalEntry{
		Seq:    seq,
		Action: "add",
		Conn:   conn,
	}
	journal = append(journal, entry)
	if len(journal) > maxJournal {
		journal = journal[len(journal)-maxJournal:]
	}
	mu.Unlock()

	scheduleNotify()
}

func RemoveActive(id string) {
	if _, ok := activeMap.Load(id); !ok {
		return
	}
	activeMap.Delete(id)

	mu.Lock()
	seq := version.Add(1)
	entry := JournalEntry{
		Seq:    seq,
		Action: "remove",
		ID:     id,
	}
	journal = append(journal, entry)
	if len(journal) > maxJournal {
		journal = journal[len(journal)-maxJournal:]
	}
	mu.Unlock()

	scheduleNotify()
}

func GetActiveConns() []ActiveConn {
	var result []ActiveConn
	activeMap.Range(func(_, value any) bool {
		conn := value.(*ActiveConn)
		result = append(result, *conn)
		return true
	})
	sort.Slice(result, func(i, j int) bool {
		return result[i].StartTime.Before(result[j].StartTime)
	})
	return result
}

func GetActiveConnsAfterSeq(seq uint64) (entries []JournalEntry, conns []ActiveConn, stale bool) {
	mu.RLock()
	journalLen := len(journal)
	firstSeq := uint64(0)
	if journalLen > 0 {
		firstSeq = journal[0].Seq
	}
	mu.RUnlock()

	if journalLen == 0 || seq < firstSeq || seq > version.Load() {
		// seq newer than anything this process issued (seq > version)
		// means the version counter reset — the client cursor is from
		// before a process restart, so a full snapshot is the only
		// correct answer; journal seqs no longer reach that cursor.
		activeMap.Range(func(_, value any) bool {
			conn := value.(*ActiveConn)
			conns = append(conns, *conn)
			return true
		})
		sort.Slice(conns, func(i, j int) bool {
			return conns[i].StartTime.Before(conns[j].StartTime)
		})
		return nil, conns, true
	}

	mu.RLock()
	for _, e := range journal {
		if e.Seq > seq {
			entries = append(entries, e)
		}
	}
	mu.RUnlock()
	return entries, nil, false
}

func GetActiveCount() int {
	count := 0
	activeMap.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
