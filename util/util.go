package util

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	"phaethon/config"

	"log"
	"time"
)

// Sha224Hex computes SHA-224 hex string (56 chars)
func Sha224Hex(input string) string {
	h := sha256.New224()
	h.Write([]byte(input))
	return hex.EncodeToString(h.Sum(nil))
}

// Relay bidirectionally copies data between two connections.
func Relay(left, right net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	copy := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		// Signal the other side to finish
		type closeWriter interface {
			CloseWrite() error
		}
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}

	go copy(left, right)
	go copy(right, left)

	wg.Wait()
}

// RelayWithRateLimit relays with rate limiting.
func RelayWithRateLimit(left, right net.Conn, upLimiter, downLimiter *config.RateLimiter) {
	if upLimiter == nil && downLimiter == nil {
		Relay(left, right)
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)

	rateCopy := func(dst, src net.Conn, limiter *config.RateLimiter) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if limiter != nil {
					delay := limiter.Schedule(n)
					if delay > 0 {
						time.Sleep(delay)
					}
				}
				_, werr := dst.Write(buf[:n])
				if werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		type closeWriter interface {
			CloseWrite() error
		}
		if cw, ok := dst.(closeWriter); ok {
			cw.CloseWrite()
		} else if tc, ok := dst.(*net.TCPConn); ok {
			tc.CloseWrite()
		} else {
			dst.Close()
		}
	}

	go rateCopy(right, left, upLimiter)   // left→right = upstream
	go rateCopy(left, right, downLimiter) // right→left = downstream

	wg.Wait()
}

// GenerateSelfSignedCert generates a self-signed TLS certificate.
func GenerateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Phaethon"},
		},
		NotBefore: time.Now().Add(-time.Hour),
		NotAfter:  time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	return tls.Certificate{
		Certificate: [][]byte{certDER},
		PrivateKey:  key,
	}, nil
}

func Min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ReadLine reads a single line (up to '\n') from r and returns the line content
// without the trailing newline or carriage return. Returns io.EOF on empty read.
func ReadLine(r io.Reader) (string, error) {
	var buf [4096]byte
	var line []byte
	for {
		n, err := r.Read(buf[:])
		if n == 0 {
			if err != nil {
				if len(line) == 0 {
					return "", err
				}
				return string(line), nil // return partial line on EOF
			}
			continue
		}
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				line = append(line, buf[:i]...)
				// Strip trailing \r if present
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				return string(line), nil
			}
		}
		line = append(line, buf[:n]...)
		if len(line) > 65536 {
			return "", fmt.Errorf("readline: line too long")
		}
	}
}

// Logger helper
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

var currentLogLevel atomic.Int32

func init() {
	currentLogLevel.Store(int32(LogLevelInfo))
}

var Logger = log.New(log.Writer(), "[phaethon] ", log.LstdFlags)

func SetLogLevel(level LogLevel) {
	currentLogLevel.Store(int32(level))
}

func LogDebug(format string, v ...interface{}) {
	if LogLevel(currentLogLevel.Load()) <= LogLevelDebug {
		Logger.Printf("[DEBUG] "+format, v...)
	}
}

func LogInfo(format string, v ...interface{}) {
	if LogLevel(currentLogLevel.Load()) <= LogLevelInfo {
		Logger.Printf("[INFO] "+format, v...)
	}
}

func LogWarn(format string, v ...interface{}) {
	if LogLevel(currentLogLevel.Load()) <= LogLevelWarn {
		Logger.Printf("[WARN] "+format, v...)
	}
}

func LogError(format string, v ...interface{}) {
	if LogLevel(currentLogLevel.Load()) <= LogLevelError {
		Logger.Printf("[ERROR] "+format, v...)
	}
}

var connIDCounter uint64

// NextConnID generates a unique connection identifier for correlating
// logs across the inbound accept, outbound dial, and relay phases.
func NextConnID() string {
	return fmt.Sprintf("conn-%d", atomic.AddUint64(&connIDCounter, 1))
}

// SetTCPNoDelay disables Nagle's algorithm on a TCP connection (including
// those wrapped in TLS) so small interactive packets are sent immediately
// without being buffered for an ACK, avoiding ~40ms stalls.
func SetTCPNoDelay(conn net.Conn) {
	switch c := conn.(type) {
	case *net.TCPConn:
		c.SetNoDelay(true)
	case *tls.Conn:
		if nc := c.NetConn(); nc != nil {
			if tc, ok := nc.(*net.TCPConn); ok {
				tc.SetNoDelay(true)
			}
		}
	}
}

// FIFOSet is a fixed-capacity FIFO set with TTL: Put returns true if the key
// was unseen or its previous entry has expired. When full, evicts the oldest.
type FIFOSet struct {
	keys  []string
	index map[string]int
	times []time.Time
	pos   int
	ttl   time.Duration
}

// NewFIFOSet creates a new FIFOSet with given capacity and default 5-minute TTL.
func NewFIFOSet(cap int) *FIFOSet {
	return &FIFOSet{
		keys:  make([]string, cap),
		index: make(map[string]int, cap),
		times: make([]time.Time, cap),
		ttl:   5 * time.Minute,
	}
}

// SetTTL overrides the default TTL.
func (s *FIFOSet) SetTTL(d time.Duration) { s.ttl = d }

// Put adds a key. Returns true if the key was unseen or expired (first time since TTL).
func (s *FIFOSet) Put(key string) bool {
	now := time.Now()
	if i, ok := s.index[key]; ok {
		if now.Sub(s.times[i]) < s.ttl {
			return false // still fresh
		}
		// expired — delete old slot so we can re-insert below
		delete(s.index, key)
	}
	if len(s.index) >= cap(s.keys) {
		old := s.keys[s.pos]
		delete(s.index, old)
	}
	s.keys[s.pos] = key
	s.times[s.pos] = now
	s.index[key] = s.pos
	s.pos = (s.pos + 1) % cap(s.keys)
	return true
}

// IsClosedErr returns true if err is a "use of closed network connection" error.
// These occur during normal shutdown and should not be logged as warnings.
func IsClosedErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "use of closed network connection")
}
