package p2p

import (
	"errors"
	"sync"
)

// udpLegReturnPath is the relay-bound return writer for one leg endpoint.
// Pointer identity lets a closing relay clear only its own binding.
type udpLegReturnPath struct {
	write func(datagram []byte) error
}

// udpLegEndpoint is the passive side's "logical listener" for one leg port P
// (design §3: 这不是 OS 层面的监听). The socks5 UDP relay delivers datagrams
// addressed to 0.0.0.0:P here and binds the return-path writer on first use.
type udpLegEndpoint struct {
	port      int
	inboundCh chan udpLegInbound // datagrams copied by the relay
	done      chan struct{}
	closeOnce sync.Once

	mu sync.RWMutex
	rp *udpLegReturnPath
}

type udpLegInbound struct {
	datagram []byte // owned copy of the frame datagram
	srcAddr  string // client's UDP socket address (for FSM double-condition)
}

var errUDPLegNoReturnPath = errors.New("udp leg: no return path bound")

func (ep *udpLegEndpoint) deliver(dgram []byte, srcAddr string) {
	select {
	case <-ep.done:
	case ep.inboundCh <- udpLegInbound{datagram: dgram, srcAddr: srcAddr}:
	default:
		// leg queue full: drop (datagram semantics)
	}
}

func (ep *udpLegEndpoint) bindReturnPath(rp *udpLegReturnPath) {
	ep.mu.Lock()
	ep.rp = rp
	ep.mu.Unlock()
}

// clearReturnPath removes rp only if it is still the bound writer.
func (ep *udpLegEndpoint) clearReturnPath(rp *udpLegReturnPath) {
	ep.mu.Lock()
	if ep.rp == rp {
		ep.rp = nil
	}
	ep.mu.Unlock()
}

func (ep *udpLegEndpoint) send(dgram []byte) error {
	ep.mu.RLock()
	rp := ep.rp
	ep.mu.RUnlock()
	if rp == nil {
		return errUDPLegNoReturnPath
	}
	return rp.write(dgram)
}

func (ep *udpLegEndpoint) close() {
	ep.closeOnce.Do(func() { close(ep.done) })
}

// udpLegRegistry maps allocated leg ports to endpoints. Global: the socks5
// UDP relay (server package) looks up ports here; sessions register/unregister.
type udpLegRegistry struct {
	mu    sync.Mutex
	ports map[int]*udpLegEndpoint
	next  int
}

const (
	udpLegPortMin = 40000
	udpLegPortMax = 40999
)

var globalUDPLegRegistry = newUDPLegRegistry()

func newUDPLegRegistry() *udpLegRegistry {
	return &udpLegRegistry{ports: make(map[int]*udpLegEndpoint), next: udpLegPortMin}
}

func (r *udpLegRegistry) allocate() *udpLegEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i <= udpLegPortMax-udpLegPortMin; i++ {
		port := r.next
		r.next++
		if r.next > udpLegPortMax {
			r.next = udpLegPortMin
		}
		if _, taken := r.ports[port]; taken {
			continue
		}
		ep := &udpLegEndpoint{
			port:      port,
			inboundCh: make(chan udpLegInbound, 512),
			done:      make(chan struct{}),
		}
		r.ports[port] = ep
		return ep
	}
	return nil
}

func (r *udpLegRegistry) lookup(port int) *udpLegEndpoint {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ports[port]
}

func (r *udpLegRegistry) release(port int) {
	r.mu.Lock()
	ep, ok := r.ports[port]
	if ok {
		delete(r.ports, port)
	}
	r.mu.Unlock()
	if ok {
		ep.close()
	}
}

// UDPLegDeliver routes a relay-received datagram to the leg endpoint
// registered for port (server package bridge). The datagram is copied;
// returns false when no endpoint owns the port.
func UDPLegDeliver(port int, datagram []byte, srcAddr string) bool {
	ep := globalUDPLegRegistry.lookup(port)
	if ep == nil {
		return false
	}
	dgram := make([]byte, len(datagram))
	copy(dgram, datagram)
	ep.deliver(dgram, srcAddr)
	return true
}

// UDPLegBindReturnPath binds the relay-bound return writer for port and
// returns an unbind closure (pointer-identity safe against a newer relay)
// plus whether the port exists.
func UDPLegBindReturnPath(port int, write func(datagram []byte) error) (func(), bool) {
	ep := globalUDPLegRegistry.lookup(port)
	if ep == nil {
		return nil, false
	}
	rp := &udpLegReturnPath{write: write}
	ep.bindReturnPath(rp)
	return func() { ep.clearReturnPath(rp) }, true
}
