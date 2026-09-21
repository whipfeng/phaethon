package connlog

import (
	"phaethon/config"
)

// Record tracks one forwarded connection or datagram session through its
// lifecycle. All forward paths (TUN TCP/UDP, trojan, socks5, htunnel, mesh
// relay) use it instead of calling Log/TrackActive/RemoveActive directly, so
// recording semantics stay uniform and protocol handlers only parse requests.
type Record struct {
	inbound     string
	protocol    string
	srcAddr     string
	originalDst string
	dst         string
	port        int
	matchResult *config.MatchResult
	connID      string
	established bool
	finished    bool
}

func Start(inbound, protocol, srcAddr, originalDst string) *Record {
	return &Record{
		inbound:     inbound,
		protocol:    protocol,
		srcAddr:     srcAddr,
		originalDst: originalDst,
	}
}

// Resolve records the destination after resolver rewrite and rule matching.
func (r *Record) Resolve(dst string, port int, matchResult *config.MatchResult) *Record {
	r.dst, r.port, r.matchResult = dst, port, matchResult
	return r
}

// SetDst overrides the display destination, e.g. after a DIRECT dial resolved
// the host to a concrete IP.
func (r *Record) SetDst(addr string) *Record {
	r.dst = addr
	return r
}

// Reject logs the connection as rejected by a REJECT rule. Terminal.
func (r *Record) Reject() {
	r.log("reject", nil)
}

// Fail logs the connection as failed after dialing. Terminal.
func (r *Record) Fail(err error) {
	r.log("fail", err)
}

// Establish logs the connection as ok and starts active-connection tracking.
// When direct is true the match result is reconciled to DIRECT (preserving
// Rule/TimeRange); otherwise a group-resolved proxy differing from the rule
// name is recorded as ActualProxy. Terminal.
func (r *Record) Establish(connID string, proxy *config.Proxy, direct bool) {
	r.connID = connID
	if direct {
		if r.matchResult != nil {
			r.matchResult.ProxyName = "DIRECT"
		} else {
			r.matchResult = &config.MatchResult{ProxyName: "DIRECT"}
		}
	} else if proxy != nil && r.matchResult != nil && proxy.Name != r.matchResult.ProxyName {
		r.matchResult.ActualProxy = proxy.Name
	}
	r.established = true
	r.log("ok", nil)
}

// Close removes the active-connection entry. Safe to defer unconditionally.
func (r *Record) Close() {
	if r.established && r.connID != "" {
		RemoveActive(r.connID)
	}
}

func (r *Record) log(status string, err error) {
	if r.finished {
		return
	}
	r.finished = true
	Log(r.inbound, r.protocol, r.srcAddr, r.originalDst, r.dst, r.port, r.matchResult, status, err)
}
