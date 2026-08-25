package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/dialer"
	"phaethon/reverse"
	"phaethon/util"
)

const (
	htHeaderConnectionID = "X-I"
	htHeaderTargetHost   = "X-H"
	htHeaderTargetPort   = "X-P"
	htHeaderContentSeq   = "X-S"
	htHeaderCommand      = "X-C"
)

var errReadTimeout = errors.New("read timeout")

const maxUDPReplies = 256

// HTunnelServer handles HTTP tunnel protocol (server side)
type HTunnelServer struct {
	BaseServer
	Password string

	idGen    int64
	channels sync.Map // int64 -> *htChannel
}

// pendingOp tracks whether an operation for the current seq is already in
// flight. If so, subsequent goroutines wait on the channel and share the
// result — matching Java's broadcast semantics.
type pendingOp struct {
	active bool
	done   chan struct{}
}

type htChannel struct {
	id         int64
	connID     string
	connSeq    int
	readSeq    int
	writeSeq   int
	targetConn net.Conn
	mu         sync.Mutex

	address   string
	port      int
	isReverse bool
	isUDP     bool // UDP tunnel mode

	// Cached results for step==0 retry
	lastReadData []byte
	lastWriteOk  bool

	// Pending ops for shared execution (broadcast semantics)
	connPend  pendingOp
	readPend  pendingOp
	writePend pendingOp
	hbPend    pendingOp

	// AEAD encryption (XChaCha20-Poly1305, nil when no password)
	crypto *util.HTunnelCrypto

	// For reverse connections: virtual net.Conn bridged over HTTP GET/POST
	revConn *htunnelServerConn

	// UDP reply queue and downstream proxy conns (only used when isUDP is true)
	udpReplies   [][]byte // each element = [srcAddrHeader][data]
	udpReplyMu   sync.Mutex
	udpReplyCond *sync.Cond
	proxyConns   map[string]*udpProxyConn // proxy name -> downstream PacketConn
	proxyMu      sync.Mutex

	// request timeout: close channel if no request arrives within this duration
	reqTimeout *time.Timer

	rBuf [64 * 1024]byte // pre-allocated read buffer for readSource

	closed    chan struct{}
	closeOnce sync.Once
}

// leaderOrWait checks if this goroutine should lead the operation for the
// current seq. If another goroutine is already handling it, this goroutine
// waits for completion and returns false (with ch.mu held).
// If not, it marks the operation active and returns true (with ch.mu released).
func (ch *htChannel) leaderOrWait(p *pendingOp) bool {
	if p.active {
		d := p.done
		ch.mu.Unlock()
		<-d
		ch.mu.Lock()
		return false
	}
	p.active = true
	p.done = make(chan struct{})
	ch.mu.Unlock()
	return true
}

// broadcast completes a pending operation and wakes all waiters. Must hold ch.mu.
func (ch *htChannel) broadcast(p *pendingOp) {
	p.active = false
	close(p.done)
}

// htunnelServerConn implements net.Conn for reverse connections over h_tunnel.
// It bridges HTTP GET/POST request/response cycles to Read/Write, so reverse
// protocol (PING/PONG/PENG) and subsequent data flow through the HTTP tunnel
// exactly like Java's HTunnelChannel — no hijack needed.
type htunnelServerConn struct {
	ch  *htChannel
	srv *HTunnelServer

	// tx: reverse server Write() -> HTTP GET response body
	txBuf   []byte
	txMu    sync.Mutex
	txReady chan struct{}

	// rx: HTTP POST body -> reverse server Read()
	rxBuf   []byte
	rxOff   int
	rxMu    sync.Mutex
	rxReady chan struct{}

	readDeadline   time.Time
	readDeadlineMu sync.Mutex

	closed   bool
	closeMu  sync.Mutex
	closeCh_ chan struct{}
}

func newHTunnelServerConn(ch *htChannel, srv *HTunnelServer) *htunnelServerConn {
	return &htunnelServerConn{
		ch:       ch,
		srv:      srv,
		txReady:  make(chan struct{}, 1),
		rxReady:  make(chan struct{}, 1),
		closeCh_: make(chan struct{}),
	}
}

func (c *htunnelServerConn) Read(b []byte) (int, error) {
	for {
		c.rxMu.Lock()
		if c.rxOff < len(c.rxBuf) {
			n := copy(b, c.rxBuf[c.rxOff:])
			c.rxOff += n
			c.rxMu.Unlock()
			return n, nil
		}
		c.rxMu.Unlock()

		c.readDeadlineMu.Lock()
		deadline := c.readDeadline
		c.readDeadlineMu.Unlock()

		var timerCh <-chan time.Time
		if !deadline.IsZero() {
			d := time.Until(deadline)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timerCh = time.After(d)
		}

		select {
		case <-c.rxReady:
			continue
		case <-c.closeCh_:
			return 0, io.EOF
		case <-timerCh:
			return 0, os.ErrDeadlineExceeded
		}
	}
}

func (c *htunnelServerConn) Write(b []byte) (int, error) {
	c.txMu.Lock()
	c.closeMu.Lock()
	isClosed := c.closed
	c.closeMu.Unlock()
	if isClosed {
		c.txMu.Unlock()
		return 0, io.ErrClosedPipe
	}
	c.txBuf = append(c.txBuf, b...)
	c.txMu.Unlock()
	select {
	case c.txReady <- struct{}{}:
	default:
	}
	return len(b), nil
}

// onGet is called by the HTTP GET handler to fetch data written by reverse server.
func (c *htunnelServerConn) onGet() ([]byte, bool) {
	c.txMu.Lock()
	c.closeMu.Lock()
	isClosed := c.closed
	c.closeMu.Unlock()
	if isClosed {
		c.txMu.Unlock()
		return nil, false
	}
	data := c.txBuf
	c.txBuf = nil
	c.txMu.Unlock()
	return data, true
}

// waitForTx blocks up to timeout waiting for data to be written.
func (c *htunnelServerConn) waitForTx(timeout time.Duration) ([]byte, bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		data, active := c.onGet()
		if !active {
			return nil, false
		}
		if len(data) > 0 {
			return data, true
		}
		select {
		case <-c.txReady:
			continue
		case <-timer.C:
			return nil, true
		case <-c.closeCh_:
			return nil, false
		}
	}
}

// onPost is called by the HTTP POST handler to deliver data to reverse server.
func (c *htunnelServerConn) onPost(data []byte) {
	c.rxMu.Lock()
	c.rxBuf = append(c.rxBuf[c.rxOff:], data...)
	c.rxOff = 0
	c.rxMu.Unlock()
	select {
	case c.rxReady <- struct{}{}:
	default:
	}
}

func (c *htunnelServerConn) Close() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.closeCh_)
	// Do NOT delete from channels here; closeChannel is the single point
	// of cleanup to avoid races where closeChannel cannot find the channel
	// to clean up targetConn and other resources.
	return nil
}

func (c *htunnelServerConn) LocalAddr() net.Addr           { return &net.TCPAddr{} }
func (c *htunnelServerConn) RemoteAddr() net.Addr          { return &net.TCPAddr{} }
func (c *htunnelServerConn) SetDeadline(t time.Time) error { return nil }
func (c *htunnelServerConn) SetReadDeadline(t time.Time) error {
	c.readDeadlineMu.Lock()
	c.readDeadline = t
	c.readDeadlineMu.Unlock()
	select {
	case c.rxReady <- struct{}{}:
	default:
	}
	return nil
}
func (c *htunnelServerConn) SetWriteDeadline(t time.Time) error { return nil }

func (c *htunnelServerConn) CloseWrite() error {
	return nil
}

func (s *HTunnelServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	contentSeq, _ := strconv.Atoi(r.Header.Get(htHeaderContentSeq))

	switch r.Method {
	case "TRACE":
		w.WriteHeader(200)
		fmt.Fprintf(w, "%s/%s", r.RemoteAddr, time.Now().String())
		return

	case "HEAD":
		if contentSeq == 0 {
			s.handleConnectionRequest(w, r)
		} else {
			s.handleConnectionPush(w, r, contentSeq)
		}

	case "PUT":
		s.handleHeartbeat(w, r)

	case "GET":
		s.handleRead(w, r)

	case "POST":
		s.handleWrite(w, r)

	case "DELETE":
		s.handleClose(w, r)

	default:
		w.WriteHeader(400)
	}
}

func (s *HTunnelServer) handleConnectionRequest(w http.ResponseWriter, r *http.Request) {
	encHost := r.Header.Get(htHeaderTargetHost)
	encPort := r.Header.Get(htHeaderTargetPort)
	if encHost == "" || encPort == "" {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	crypto := util.NewHTunnelCrypto(s.Password)
	dstHost, err := crypto.OpenHeader(encHost)
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}
	dstPort, err := crypto.OpenHeader(encPort)
	if err != nil {
		http.Error(w, "", http.StatusBadRequest)
		return
	}

	dstPortInt, _ := strconv.Atoi(dstPort)

	// Explicit bind semantics: only X-C: BIND indicates reverse connection.
	// No fallback to dstPort==0.
	cmd := r.Header.Get(htHeaderCommand)
	isReverse := cmd == "BIND"

	id := atomic.AddInt64(&s.idGen, 1)
	connID := util.NextConnID()
	ch := &htChannel{
		id:        id,
		connID:    connID,
		closed:    make(chan struct{}),
		address:   dstHost,
		port:      dstPortInt,
		isReverse: isReverse,
		isUDP:     cmd == "UDP",
		crypto:    util.NewHTunnelCrypto(s.Password),
	}
	ch.udpReplyCond = sync.NewCond(&ch.udpReplyMu)
	ch.proxyConns = make(map[string]*udpProxyConn)
	s.channels.Store(id, ch)

	// Conn timeout: 10s; will be replaced by resetReqTimeout on first real request
	ch.reqTimeout = time.AfterFunc(10*time.Second, func() {
		ch.mu.Lock()
		hasConn := ch.targetConn != nil || ch.revConn != nil
		ch.mu.Unlock()
		if !hasConn {
			s.closeChannel(id)
		}
	})

	w.Header().Set(htHeaderConnectionID, strconv.FormatInt(id, 10))
	w.WriteHeader(200)
	if ch.isUDP {
		util.LogInfo("[HT-SVR] [%s] [%s] UDP tunnel started (%s:%d)", s.Mapping.Name, connID, dstHost, dstPortInt)
	} else {
		util.LogInfo("[HT-SVR] [%s] [%s] TCP tunnel started -> %s:%d", s.Mapping.Name, connID, dstHost, dstPortInt)
	}
}

func (s *HTunnelServer) handleConnectionPush(w http.ResponseWriter, r *http.Request, contentSeq int) {
	id := s.getConnectionID(r)
	chI, ok := s.channels.Load(id)
	if !ok {
		w.WriteHeader(410)
		return
	}
	ch := chI.(*htChannel)
	ch.resetReqTimeout(s, id)

	for {
		ch.mu.Lock()
		step := contentSeq - ch.connSeq
		if step == 0 {
			if ch.connPend.active {
				ch.leaderOrWait(&ch.connPend)
				ch.mu.Unlock()
				continue // recheck step after wait
			}
			select {
			case <-ch.closed:
				ch.mu.Unlock()
				w.WriteHeader(410)
				return
			default:
			}
			active := ch.targetConn != nil || ch.revConn != nil || ch.isUDP
			ch.mu.Unlock()
			if active {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(410)
			}
			return
		}
		if step != 1 {
			ch.mu.Unlock()
			w.WriteHeader(410)
			return
		}
		if ch.leaderOrWait(&ch.connPend) {
			break
		}
		// Waiter returns with lock held; loop back to recheck step
	}

	var connOk, failed bool

	ch.mu.Lock()
	isReverse := ch.isReverse
	isUDP := ch.isUDP
	address := ch.address
	port := ch.port
	hasTarget := ch.targetConn != nil
	hasRev := ch.revConn != nil
	ch.mu.Unlock()

	if contentSeq == 1 {
		if isReverse {
			if port == reverse.BindPortControl {
				handleControlConnection(newHTunnelServerConn(ch, s), address)
				connOk = true
				return
			}
			if port != reverse.BindPortData {
				util.LogInfo("[HTUNNEL-SVR] [%s] reverse rejected: invalid port %d (only 0 or 1 allowed)", s.Mapping.Name, port)
				return
			}
			if !s.RuleConf.HasReverseAddress(address) {
				return // address not supported, close connection
			}
			ch.mu.Lock()
			if ch.revConn == nil {
				revConn := newHTunnelServerConn(ch, s)
				ch.revConn = revConn
				go reverse.HandleReverseConnection(revConn, ch.address)
			}
			ch.mu.Unlock()
			connOk = true
		} else if isUDP {
			// UDP mode: no pre-created connection needed; each packet is matched and routed independently
			connOk = true
		} else {
			if !hasTarget {
				targetConn, err := connectHTTarget(s.RuleConf, s.Mapping, address, port, ch.connID)
				if err != nil {
					util.LogError("[HT-SVR] [%s] [%s] connect target fail %s:%d: %v", s.Mapping.Name, ch.connID, address, port, err)
					failed = true
				} else {
					ch.mu.Lock()
					ch.targetConn = targetConn
					ch.mu.Unlock()
					connOk = true
				}
			} else {
				connOk = true
			}
		}
	} else if contentSeq == 2 {
		connOk = hasTarget || hasRev || isUDP
	}

	ch.mu.Lock()
	if connOk || failed {
		ch.connSeq++
	}
	ch.broadcast(&ch.connPend)
	ch.mu.Unlock()

	if connOk {
		w.WriteHeader(200)
	} else if failed {
		s.closeChannel(id)
		w.WriteHeader(410)
	} else {
		w.WriteHeader(410)
	}
}

func (s *HTunnelServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	id := s.getConnectionID(r)
	contentSeq := s.getContentSeq(r)
	chI, ok := s.channels.Load(id)
	if !ok {
		w.WriteHeader(410)
		return
	}
	ch := chI.(*htChannel)
	ch.resetReqTimeout(s, id)

	for {
		ch.mu.Lock()
		step := contentSeq - ch.connSeq
		if step == 0 {
			if ch.hbPend.active {
				ch.leaderOrWait(&ch.hbPend)
				ch.mu.Unlock()
				continue
			}
			select {
			case <-ch.closed:
				ch.mu.Unlock()
				w.WriteHeader(410)
				return
			default:
			}
			ch.mu.Unlock()
			w.WriteHeader(200)
			return
		}
		if step != 1 {
			ch.mu.Unlock()
			w.WriteHeader(410)
			return
		}
		if ch.leaderOrWait(&ch.hbPend) {
			break
		}
	}

	select {
	case <-time.After(10 * time.Second):
	case <-ch.closed:
	}

	ch.mu.Lock()
	ch.connSeq++
	ch.broadcast(&ch.hbPend)
	ch.mu.Unlock()

	select {
	case <-ch.closed:
		w.WriteHeader(410)
	default:
		w.WriteHeader(200)
	}
}

// readSource reads data from the channel's underlying source (revConn or targetConn).
// For UDP mode it reads from the udpReplyQueue.
// It must be called without holding ch.mu.
func (ch *htChannel) readSource(timeout time.Duration) ([]byte, error) {
	ch.mu.Lock()
	isUDP := ch.isUDP
	revConn := ch.revConn
	targetConn := ch.targetConn
	ch.mu.Unlock()

	if isUDP {
		return ch.readUDPSource(timeout)
	}

	if revConn != nil {
		data, active := revConn.onGet()
		if !active {
			return nil, io.EOF
		}
		if len(data) == 0 {
			data, active = revConn.waitForTx(timeout)
			if !active {
				return nil, io.EOF
			}
			if len(data) == 0 {
				return nil, errReadTimeout
			}
		}
		return data, nil
	}

	if targetConn == nil {
		return nil, errReadTimeout
	}

	targetConn.SetReadDeadline(time.Now().Add(timeout + 5*time.Second))
	n, err := targetConn.Read(ch.rBuf[:])
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return nil, errReadTimeout
		}
		return nil, io.EOF
	}
	return ch.rBuf[:n], nil
}

func (ch *htChannel) readUDPSource(timeout time.Duration) ([]byte, error) {
	ch.udpReplyMu.Lock()
	defer ch.udpReplyMu.Unlock()

	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-ch.closed:
			return nil, io.EOF
		default:
		}

		if len(ch.udpReplies) > 0 {
			data := ch.udpReplies[0]
			ch.udpReplies = ch.udpReplies[1:]
			return data, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, errReadTimeout
		}

		// Wait releases mu until Signal/Broadcast or timeout
		timer := time.AfterFunc(remaining, func() { ch.udpReplyCond.Broadcast() })
		ch.udpReplyCond.Wait()
		timer.Stop()
	}
}

func (s *HTunnelServer) handleRead(w http.ResponseWriter, r *http.Request) {
	id := s.getConnectionID(r)
	contentSeq := s.getContentSeq(r)
	chI, ok := s.channels.Load(id)
	if !ok {
		w.WriteHeader(410)
		return
	}
	ch := chI.(*htChannel)
	ch.resetReqTimeout(s, id)

	for {
		ch.mu.Lock()
		step := contentSeq - ch.readSeq
		if step == 0 {
			if ch.readPend.active {
				ch.leaderOrWait(&ch.readPend)
				ch.mu.Unlock()
				continue
			}
			data := ch.lastReadData
			ch.mu.Unlock()
			if data != nil {
				w.WriteHeader(200)
				w.Write(data)
			} else {
				w.WriteHeader(408)
			}
			return
		}
		if step != 1 {
			ch.mu.Unlock()
			w.WriteHeader(410)
			return
		}
		if ch.leaderOrWait(&ch.readPend) {
			break
		}
	}

	data, err := ch.readSource(30 * time.Second)

	ch.mu.Lock()
	ch.readSeq++
	var responseData []byte
	if err == nil {
		responseData = ch.crypto.SealBody(data)
		ch.lastReadData = responseData
	} else {
		ch.lastReadData = nil
	}
	ch.broadcast(&ch.readPend)
	ch.mu.Unlock()

	switch {
	case err == io.EOF:
		w.WriteHeader(410)
	case err == errReadTimeout:
		w.WriteHeader(408)
	case err != nil:
		w.WriteHeader(410)
	default:
		w.WriteHeader(200)
		w.Write(responseData)
	}
}

func (s *HTunnelServer) handleWrite(w http.ResponseWriter, r *http.Request) {
	id := s.getConnectionID(r)
	contentSeq := s.getContentSeq(r)
	chI, ok := s.channels.Load(id)
	if !ok {
		w.WriteHeader(410)
		return
	}
	ch := chI.(*htChannel)
	ch.resetReqTimeout(s, id)

	for {
		ch.mu.Lock()
		step := contentSeq - ch.writeSeq
		if step == 0 {
			if ch.writePend.active {
				ch.leaderOrWait(&ch.writePend)
				ch.mu.Unlock()
				continue
			}
			lastOk := ch.lastWriteOk
			ch.mu.Unlock()
			if lastOk {
				w.WriteHeader(200)
			} else {
				w.WriteHeader(408)
			}
			return
		}
		if step != 1 {
			ch.mu.Unlock()
			w.WriteHeader(410)
			return
		}
		if ch.leaderOrWait(&ch.writePend) {
			break
		}
	}

	data, err := io.ReadAll(r.Body)
	if err != nil {
		ch.mu.Lock()
		ch.writeSeq++
		ch.lastWriteOk = false
		ch.broadcast(&ch.writePend)
		ch.mu.Unlock()
		w.WriteHeader(410)
		return
	}
	if len(data) > 0 {
		var err error
		data, err = ch.crypto.OpenBody(data)
		if err != nil {
			ch.mu.Lock()
			ch.writeSeq++
			ch.lastWriteOk = false
			ch.broadcast(&ch.writePend)
			ch.mu.Unlock()
			w.WriteHeader(410)
			return
		}
	}

	ch.mu.Lock()
	isUDP := ch.isUDP
	revConn := ch.revConn
	targetConn := ch.targetConn
	ch.mu.Unlock()

	var writeOk bool
	if revConn != nil {
		if len(data) > 0 {
			revConn.onPost(data)
		}
		writeOk = true
	} else if isUDP {
		if len(data) > 0 {
			targetAddr, payload, err := util.ParseUDPAddrHeader(data)
			if err == nil && len(payload) > 0 {
				host, portStr, _ := net.SplitHostPort(targetAddr.String())
				port, _ := strconv.Atoi(portStr)
				req := config.NewConnectRequest(host, port)
				req = s.RuleConf.Resolving(req)
				proxy := s.RuleConf.Match(req, s.Mapping)
				if proxy != nil && strings.ToUpper(proxy.Type) != config.ProxyREJECT {
					upc, err := s.getProxyConn(ch, proxy)
					if err == nil {
						_, err = upc.pc.WriteTo(payload, targetAddr)
						writeOk = err == nil
					}
				}
			}
		} else {
			writeOk = true
		}
	} else if targetConn != nil {
		if len(data) > 0 {
			_, err := targetConn.Write(data)
			writeOk = err == nil
			if err != nil {
				targetConn.Close()
				ch.mu.Lock()
				if ch.targetConn == targetConn {
					ch.targetConn = nil
				}
				ch.mu.Unlock()
			}
		} else {
			writeOk = true
		}
	}

	ch.mu.Lock()
	ch.writeSeq++
	ch.lastWriteOk = writeOk
	ch.broadcast(&ch.writePend)
	ch.mu.Unlock()

	if writeOk {
		w.WriteHeader(200)
	} else {
		w.WriteHeader(410)
	}
}

// closeChannel closes a channel and cleans up its resources. Safe to call multiple times.
func (s *HTunnelServer) closeChannel(id int64) {
	chI, ok := s.channels.Load(id)
	if !ok {
		return
	}
	ch := chI.(*htChannel)

	ch.closeOnce.Do(func() {
		if ch.isUDP {
			util.LogInfo("[HT-SVR] [%s] [%s] UDP tunnel closed (%s:%d)", s.Mapping.Name, ch.connID, ch.address, ch.port)
		}
		close(ch.closed)
		if ch.udpReplyCond != nil {
			ch.udpReplyCond.Broadcast()
		}
		ch.mu.Lock()
		if ch.targetConn != nil {
			ch.targetConn.Close()
			ch.targetConn = nil
		}
		if ch.revConn != nil {
			ch.revConn.Close()
			ch.revConn = nil
		}
		ch.mu.Unlock()
		ch.proxyMu.Lock()
		for _, upc := range ch.proxyConns {
			upc.cancel()
			upc.pc.Close()
		}
		ch.proxyConns = nil
		ch.proxyMu.Unlock()
		if ch.reqTimeout != nil {
			ch.reqTimeout.Stop()
		}
	})
	s.channels.Delete(id)
}

// resetReqTimeout resets the 30s request-timeout timer. Call on every request arrival.
func (ch *htChannel) resetReqTimeout(s *HTunnelServer, id int64) {
	ch.mu.Lock()
	if ch.reqTimeout != nil {
		ch.reqTimeout.Stop()
	}
	ch.reqTimeout = time.AfterFunc(30*time.Second, func() {
		s.closeChannel(id)
	})
	ch.mu.Unlock()
}

func (s *HTunnelServer) handleClose(w http.ResponseWriter, r *http.Request) {
	id := s.getConnectionID(r)
	s.closeChannel(id)
	w.WriteHeader(410)
}

func (s *HTunnelServer) getConnectionID(r *http.Request) int64 {
	idStr := r.Header.Get(htHeaderConnectionID)
	id, _ := strconv.ParseInt(idStr, 10, 64)
	return id
}

func (s *HTunnelServer) getContentSeq(r *http.Request) int {
	seq, _ := strconv.Atoi(r.Header.Get(htHeaderContentSeq))
	return seq
}

func StartHTunnel(ruleConf *config.RuleConfiguration, mapping *config.Mapping) (net.Listener, error) {
	addr := fmt.Sprintf(":%d", mapping.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}

	srv := &HTunnelServer{
		BaseServer: BaseServer{RuleConf: ruleConf, Mapping: mapping},
		Password:   mapping.Password,
	}

	httpSrv := &http.Server{
		Handler: srv,
	}
	go httpSrv.Serve(ln)
	return ln, nil
}

// getProxyConn returns a cached PacketConn for the proxy, creating one if needed.
func (s *HTunnelServer) getProxyConn(ch *htChannel, proxy *config.Proxy) (*udpProxyConn, error) {
	ch.proxyMu.Lock()
	defer ch.proxyMu.Unlock()

	if upc, ok := ch.proxyConns[proxy.Name]; ok {
		if atomic.LoadInt32(&upc.dead) == 0 {
			return upc, nil
		}
		delete(ch.proxyConns, proxy.Name)
		upc.pc.Close()
	}

	pc, err := dialer.NewUDPDialer(proxy).DialPacket()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	upc := &udpProxyConn{pc: pc, cancel: cancel}
	ch.proxyConns[proxy.Name] = upc

	// Start goroutine to read replies from this proxy and enqueue them
	go s.proxyReadLoop(ctx, ch, pc, proxy.Name)

	return upc, nil
}

// proxyReadLoop reads replies from a downstream PacketConn and appends them to
// the channel's udpReplyQueue so that handleRead can return them.
func (s *HTunnelServer) proxyReadLoop(ctx context.Context, ch *htChannel, pc net.PacketConn, proxyName string) {
	defer func() {
		ch.proxyMu.Lock()
		if upc, ok := ch.proxyConns[proxyName]; ok && upc.pc == pc {
			atomic.StoreInt32(&upc.dead, 1)
			delete(ch.proxyConns, proxyName)
		}
		ch.proxyMu.Unlock()
	}()

	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch.closed:
			return
		default:
		}

		pc.SetReadDeadline(time.Now().Add(60 * time.Second))
		n, srcAddr, err := pc.ReadFrom(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if util.IsClosedErr(err) {
				util.LogDebug("[HT-SVR] [%s] [%s] UDP proxy read closed (normal): %v", s.Mapping.Name, ch.connID, err)
			} else {
				util.LogWarn("[HT-SVR] [%s] [%s] UDP proxy read error: %v", s.Mapping.Name, ch.connID, err)
			}
			return
		}

		header, err := util.BuildUDPAddrHeader(srcAddr)
		if err != nil {
			continue
		}
		packet := make([]byte, len(header)+n)
		copy(packet, header)
		copy(packet[len(header):], buf[:n])

		ch.udpReplyMu.Lock()
		if len(ch.udpReplies) >= maxUDPReplies {
			// Drop oldest to bound memory
			ch.udpReplies = ch.udpReplies[1:]
		}
		ch.udpReplies = append(ch.udpReplies, packet)
		ch.udpReplyMu.Unlock()
		ch.udpReplyCond.Signal()
	}
}

// Simplified H_Tunnel mapping handler - connects to target based on address from HTChannel
func connectHTTarget(ruleConf *config.RuleConfiguration, mapping *config.Mapping, dstHost string, dstPort int, connID string) (net.Conn, error) {
	req := config.NewConnectRequest(dstHost, dstPort)
	req = ruleConf.Resolving(req)

	proxy := ruleConf.Match(req, mapping)
	if proxy == nil || strings.ToUpper(proxy.Type) == config.ProxyREJECT {
		return nil, fmt.Errorf("[HT-SVR] [%s] [%s] rejected %s:%d", mapping.Name, connID, dstHost, dstPort)
	}

	return dialer.ChainDialWithID(proxy, req.DstAddr, req.DstPort, connID)
}
