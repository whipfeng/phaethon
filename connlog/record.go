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
	srcPort     int
	originalDst string
	dst         string
	port        int
	matchResult *config.MatchResult
	connID      string
	via         string // mesh egress node ID, display-only
	fwdKey      string // explicit cross-node correlation key; empty = derive from display 5-tuple
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

// WithConn binds the record to a connection ID and client source port before
// Establish, so Reject/Fail paths also carry the correlation fields.
func (r *Record) WithConn(connID string, srcPort int) *Record {
	r.connID = connID
	r.srcPort = srcPort
	return r
}

// SetVia marks the mesh egress node ID (display-only) for MESH-routed flows.
func (r *Record) SetVia(nodeID string) *Record {
	r.via = nodeID
	return r
}

// SetFwdKey pins the cross-node correlation key for the flow. MeshDial passes
// the inner 5-tuple (VIP>fakeIP); direct/proxy flows leave it empty and the
// key derives from the display 5-tuple instead.
func (r *Record) SetFwdKey(key string) *Record {
	r.fwdKey = key
	return r
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
	// Track active connection after logging
	TrackActive(connID, r.inbound, r.protocol, r.srcAddr, r.originalDst, r.dst, r.port, r.matchResult,
		WithActiveSrcPort(r.srcPort), WithActiveVia(r.via), WithActiveFwdKey(r.fwdKey))
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
	opts := []LogOption{WithConnID(r.connID), WithSrcPort(r.srcPort), WithVia(r.via)}
	if r.fwdKey != "" {
		opts = append(opts, WithFwdKey(r.fwdKey))
	}
	Log(r.inbound, r.protocol, r.srcAddr, r.originalDst, r.dst, r.port, r.matchResult, status, err, opts...)
}
