package admin

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"phaethon/config"
	"phaethon/connlog"
	"phaethon/db"
	"phaethon/mesh"
	"phaethon/p2p"
	"phaethon/reverse"
	"phaethon/server"
	"phaethon/tun"
	"phaethon/util"
)

//go:embed certs/ca.crt
var defaultClientCA []byte

// adminVersion is used to bust browser caches for embedded static files.
// It is set once at process startup so a new deployment forces clients to
// re-fetch app.js / style.css / i18n.js.
var adminVersion = os.Getenv("PHAETHON_VERSION")

func init() {
	if adminVersion == "" {
		adminVersion = fmt.Sprintf("%d", time.Now().Unix())
	}
}

const (
	captchaFailedAttempts = 3
	captchaAttemptWindow  = 15 * time.Minute
	captchaTTL            = 10 * time.Minute
	captchaDigits         = 4
	captchaWidth          = 140
	captchaHeight         = 54
	captchaDigitWidth     = 24
	captchaDigitHeight    = 40
	captchaDigitPadding   = 8
)

// loginSecurity tracks per-client CAPTCHA state.
var (
	loginAttemptMu sync.Mutex
	loginAttempts  = make(map[string][]time.Time)
	captchaStoreMu sync.Mutex
	captchaStore   = make(map[string]captchaEntry)
)

type captchaEntry struct {
	code    string
	expires time.Time
}

// clientIP returns the originating client address, preferring X-Forwarded-For
// when the admin panel is served behind a reverse proxy.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.Index(fwd, ","); i != -1 {
			fwd = strings.TrimSpace(fwd[:i])
		}
		if fwd != "" {
			return fwd
		}
	}
	if ri := r.Header.Get("X-Real-Ip"); ri != "" {
		return ri
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func recordFailedLogin(ip string) {
	loginAttemptMu.Lock()
	defer loginAttemptMu.Unlock()
	cutoff := time.Now().Add(-captchaAttemptWindow)
	list := loginAttempts[ip]
	fresh := list[:0]
	for _, t := range list {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	loginAttempts[ip] = append(fresh, time.Now())
}

func clearFailedLogins(ip string) {
	loginAttemptMu.Lock()
	defer loginAttemptMu.Unlock()
	delete(loginAttempts, ip)
}

func captchaRequiredFor(ip string) bool {
	loginAttemptMu.Lock()
	defer loginAttemptMu.Unlock()
	cutoff := time.Now().Add(-captchaAttemptWindow)
	list := loginAttempts[ip]
	count := 0
	for _, t := range list {
		if t.After(cutoff) {
			count++
		}
	}
	return count >= captchaFailedAttempts
}

func storeCaptcha(ip, code string) {
	captchaStoreMu.Lock()
	defer captchaStoreMu.Unlock()
	captchaStore[ip] = captchaEntry{code: code, expires: time.Now().Add(captchaTTL)}
}

func verifyCaptcha(ip, code string) bool {
	if code == "" {
		return false
	}
	captchaStoreMu.Lock()
	defer captchaStoreMu.Unlock()
	entry, ok := captchaStore[ip]
	if !ok || time.Now().After(entry.expires) {
		return false
	}
	delete(captchaStore, ip)
	return strings.EqualFold(entry.code, code)
}

func cleanupCaptchas() {
	captchaStoreMu.Lock()
	defer captchaStoreMu.Unlock()
	now := time.Now()
	for ip, e := range captchaStore {
		if now.After(e.expires) {
			delete(captchaStore, ip)
		}
	}
}

// randomDigits generates a zero-padded string of n decimal digits using crypto/rand.
func randomDigits(n int) string {
	max := big.NewInt(1)
	for i := 0; i < n; i++ {
		max.Mul(max, big.NewInt(10))
	}
	v, err := rand.Int(rand.Reader, max)
	if err != nil {
		// Fall back to time-based pseudo-random if crypto source fails.
		v = big.NewInt(time.Now().UnixNano())
	}
	return fmt.Sprintf("%0"+strconv.Itoa(n)+"d", v.Int64())
}

// drawCaptcha renders the given digits as a 7-segment-style PNG image.
func drawCaptcha(digits string) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, captchaWidth, captchaHeight))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{245, 245, 245, 255}}, image.Point{}, draw.Src)

	// Add light noise lines.
	for i := 0; i < 8; i++ {
		c := color.RGBA{uint8(180 + i*5), uint8(180 + i*7), uint8(200), 255}
		x1 := 5 + (i*17)%captchaWidth
		y1 := 5 + (i*11)%captchaHeight
		x2 := 10 + (i*23)%(captchaWidth-10)
		y2 := 10 + (i*19)%(captchaHeight-10)
		drawRect(img, x1, y1, x2, y2, c)
	}

	startX := (captchaWidth - (captchaDigits*captchaDigitWidth + (captchaDigits-1)*captchaDigitPadding)) / 2
	startY := (captchaHeight - captchaDigitHeight) / 2
	segColor := color.RGBA{30, 30, 30, 255}
	for i, ch := range digits {
		d := ch - '0'
		if d < 0 || d > 9 {
			d = 0
		}
		x := startX + i*(captchaDigitWidth+captchaDigitPadding)
		drawDigit(img, x, startY, byte(d), segColor)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawRect(img *image.RGBA, x1, y1, x2, y2 int, c color.Color) {
	if x1 > x2 {
		x1, x2 = x2, x1
	}
	if y1 > y2 {
		y1, y2 = y2, y1
	}
	for y := y1; y <= y2; y++ {
		for x := x1; x <= x2; x++ {
			img.Set(x, y, c)
		}
	}
}

// 7-segment layout (bits: top, top-right, bottom-right, bottom, bottom-left, top-left, middle).
var digitSegments = []byte{
	0b0111111, // 0
	0b0000110, // 1
	0b1011011, // 2
	0b1001111, // 3
	0b1100110, // 4
	0b1101101, // 5
	0b1111101, // 6
	0b0000111, // 7
	0b1111111, // 8
	0b1101111, // 9
}

func drawDigit(img *image.RGBA, x, y int, d byte, c color.Color) {
	seg := digitSegments[d]
	dx := captchaDigitWidth
	dy := captchaDigitHeight
	thick := 3

	if seg&0b0000001 != 0 { // top
		drawRect(img, x+4, y+1, x+dx-4, y+thick, c)
	}
	if seg&0b0100000 != 0 { // top-left
		drawRect(img, x+1, y+4, x+thick, y+dy/2-2, c)
	}
	if seg&0b0000010 != 0 { // top-right
		drawRect(img, x+dx-thick-1, y+4, x+dx-2, y+dy/2-2, c)
	}
	if seg&0b1000000 != 0 { // middle
		drawRect(img, x+4, y+dy/2-1, x+dx-4, y+dy/2+thick-1, c)
	}
	if seg&0b0010000 != 0 { // bottom-left
		drawRect(img, x+1, y+dy/2+2, x+thick, y+dy-4, c)
	}
	if seg&0b0000100 != 0 { // bottom-right
		drawRect(img, x+dx-thick-1, y+dy/2+2, x+dx-2, y+dy-4, c)
	}
	if seg&0b0001000 != 0 { // bottom
		drawRect(img, x+4, y+dy-thick-1, x+dx-4, y+dy-2, c)
	}
}

// AdminServer provides a web management interface.
type AdminServer struct {
	config    *config.AdminConfig
	stats     *StatsCollector
	server    *http.Server
	ln        net.Listener
	tlsConfig *tls.Config // cached TLS config for ServeConn
	mu        sync.RWMutex

	// SSE broadcaster lifecycle
	sseStopCh chan struct{}
	sseWG     sync.WaitGroup
	sseLast   []byte // last published stats JSON

	// OnReload is called when the user explicitly requests a config reload
	// from the admin panel. The main package should set this to trigger
	// a full runtime resource rebuild (listeners, reverse bindings, etc.).
	OnReload func()

	// OnTUNToggle is called when the user enables/disables TUN from the admin panel.
	// The main package should set this to start or stop the TUN engine directly.
	OnTUNToggle func(enable bool) error

	// GetTUNStatus returns the current TUN availability/runtime state.
	// The main package should set this so the admin panel can display it.
	GetTUNStatus func() map[string]interface{}

	// OnDHCPStaticBindingsUpdate is called when DHCP static bindings are updated via the API.
	OnDHCPStaticBindingsUpdate func(bindings []config.DHCPStaticBinding)

	// RefreshSubscription fetches the subscription for the named subscription and
	// updates its internal node pool. Set by the main package to avoid a
	// circular dependency on the dialer package.
	RefreshSubscription func(subName string) error

	// CheckGroupHealth runs a one-off health check for the named group.
	// Set by the main package.
	CheckGroupHealth func(groupName string) error

	// CheckGroupTest runs an immediate one-off test for every member of the
	// named group, including manual proxies, and persists the result.
	// Set by the main package.
	CheckGroupTest func(groupName string) error

	// CheckGroupProxyHealth runs a one-off health check for a single proxy/node in a group.
	// Set by the main package.
	CheckGroupProxyHealth func(groupName, proxyName string) (config.HealthInfo, error)

	// CheckSubscriptionHealth runs a one-off health check for a single node
	// inside a subscription. Set by the main package.
	CheckSubscriptionHealth func(subName, nodeName, url string) (config.HealthInfo, error)

	// CheckProxyHealth runs a one-off connectivity check for a top-level proxy.
	// Set by the main package.
	CheckProxyHealth func(proxyName string) (config.HealthInfo, error)

	// GetReverseBindings returns the current dynamic reverse client bindings
	// from the registry-side ControlManager. Set by the main package on the
	// registry instance so the admin UI can show dynamically registered clients.
	GetReverseBindings func() []server.PortBinding

	// ForceRemoveBinding removes a stale dynamic reverse binding by reverse ID
	// and sequence number. Set by the main package on the registry instance.
	ForceRemoveBinding func(reverseID string, seq int) error

	// OnIncrementalUpdate is called when data-level config changes are made
	// (proxies, rules, groups, subscriptions). It updates the atomic pointer
	// without restarting listeners. Set by the main package.
	OnIncrementalUpdate func() error

	// OnMappingUpdate is called when a mapping is added/modified/deleted.
	// It receives the old mapping (nil for new) and new mapping (nil for delete).
	// The callback handles listener restart precisely - no full reload needed.
	OnMappingUpdate func(old, newMapping *config.Mapping) error

	// OnReverseConfigUpdate is called when a reverse config is added/updated/deleted.
	// It receives the old config (nil for new) and new config (nil for delete).
	// The callback handles starting/stopping reverse client goroutines.
	OnReverseConfigUpdate func(old, newConfig *config.ReverseConfig) error

	// templates
	pages *pageTemplates

	// defaultRaw holds the embedded default config bytes, used for reset/fallback.
	defaultRaw []byte

	// package upload/publish state (see package.go)
	packageUploadMu   sync.Mutex // one upload at a time
	packagePublishMu  sync.Mutex // one publish at a time (single-flight)
	packagePublishing atomic.Bool

	// sessionSecret is a per-process random fallback HMAC key for signed cookies,
	// used only when the admin token is empty. It is generated at startup so that
	// deployments without an explicit token are still protected against cookie forgery.
	sessionSecret []byte

	// Mesh package distribution
	peerLister     func() []PeerBrief                           // returns connected mesh peers
	meshDialFn     func(network, addr string) (net.Conn, error) // dials through mesh network
	dnsResolver    func(domain string) (net.IP, error)          // resolves domain through mesh DNS
	meshHTTPClient *http.Client                                 // HTTP client for mesh communication
	adminPort      int                                          // admin API port for mesh peers

	// GetCurrentVersion returns the current running version. Set by main package.
	GetCurrentVersion func() string
	// GetPlatform returns the compile-time platform identifier. Set by main package.
	GetPlatform func() string
	// GetArch returns the compile-time architecture identifier. Set by main package.
	GetArch func() string

	// OnRestart is called when the user requests a process restart from the admin panel.
	// The main package should set this to trigger a graceful exit, allowing the watchdog
	// to restart the process. Set by the main package.
	OnRestart func()
}

// PeerBrief contains brief information about a mesh peer.
type PeerBrief struct {
	NodeID string
}

// triggerReload schedules a full runtime reload without blocking the caller.
// It is safe to call while holding s.mu because the reload runs in a new
// goroutine and will wait for the lock to be released.
func (s *AdminServer) triggerReload() {
	if s.OnReload != nil {
		go s.OnReload()
	}
}

// NewAdminServer creates a new admin server instance.
func NewAdminServer(ac *config.AdminConfig, defaultRaw []byte) *AdminServer {
	if ac == nil {
		ac = &config.AdminConfig{Enabled: false}
	}
	if ac.Addr == "" {
		ac.Addr = ":39999"
	}

	s := &AdminServer{
		config:     ac,
		stats:      NewStatsCollector(),
		defaultRaw: defaultRaw,
	}

	// Load or generate a persistent session secret so sessions survive restarts.
	// If admin.token is configured, it will be used as the signing key instead.
	s.sessionSecret = s.loadOrGenerateSessionSecret()

	s.parseTemplates()
	return s
}

// SetPeerLister sets the function to list connected mesh peers.
func (s *AdminServer) SetPeerLister(f func() []PeerBrief) {
	s.peerLister = f
}

// SetMeshDialFn sets the function to dial through the mesh network.
func (s *AdminServer) SetMeshDialFn(f func(network, addr string) (net.Conn, error), dnsResolver func(domain string) (net.IP, error)) {
	s.meshDialFn = f
	s.dnsResolver = dnsResolver
	// Initialize mesh HTTP client with DNS resolution support
	s.meshHTTPClient = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, fmt.Errorf("parse addr: %w", err)
				}

				// Resolve domain name if needed
				if ip := net.ParseIP(host); ip == nil {
					// It's a domain name, resolve it
					if dnsResolver == nil {
						return nil, fmt.Errorf("DNS resolver not available for domain: %s", host)
					}
					fakeIP, err := dnsResolver(host)
					if err != nil {
						return nil, fmt.Errorf("resolve %s: %w", host, err)
					}
					addr = net.JoinHostPort(fakeIP.String(), port)
				}

				return f(network, addr)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		// Large pkg transfers over slow mesh transports shared with
		// concurrent pushes can take minutes; must exceed the server's
		// read/write budget so the server isn't the one that gives up first.
		Timeout: 300 * time.Second,
	}
}

// SetAdminPort sets the admin API port for mesh peer communication.
func (s *AdminServer) SetAdminPort(port int) {
	s.adminPort = port
}

// loadOrGenerateSessionSecret loads the session secret from a file or generates a new one.
func (s *AdminServer) loadOrGenerateSessionSecret() []byte {
	secretFile := "admin-session-secret.key"

	// Try to load existing secret
	if data, err := os.ReadFile(secretFile); err == nil && len(data) == 32 {
		util.LogModuleInfo("admin", "loaded session secret from %s", secretFile)
		return data
	}

	// Generate new secret
	secret := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		util.LogModuleError("admin", "failed to generate session secret: %v", err)
		return nil
	}

	// Save to file
	if err := os.WriteFile(secretFile, secret, 0600); err != nil {
		util.LogModuleWarn("admin", "failed to save session secret to %s: %v", secretFile, err)
	} else {
		util.LogModuleInfo("admin", "generated and saved session secret to %s", secretFile)
	}

	return secret
}

// Stats returns the stats collector for instrumentation.
func (s *AdminServer) Stats() *StatsCollector {
	return s.stats
}

// ListenAddr returns the address the admin server is listening on.
func (s *AdminServer) ListenAddr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ln != nil {
		return s.ln.Addr().String()
	}
	if s.config != nil {
		return s.config.Addr
	}
	return ""
}

// pageTemplates holds parsed templates for each page (layout + page content).
// Each entry is isolated so {{define "content"}} blocks don't conflict.
type pageTemplates struct {
	dashboard     *template.Template
	tun           *template.Template
	mesh          *template.Template
	logs          *template.Template
	connections   *template.Template
	proxies       *template.Template
	subscriptions *template.Template
	rules         *template.Template
	mappings      *template.Template
	resolvers     *template.Template
	reverseWizard *template.Template
	packagePage   *template.Template
	login         *template.Template
	setup         *template.Template
}

// templateFuncs returns the common template function map.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatBytes": formatBytes,
		"formatUptime": func(secs int64) string {
			d := time.Duration(secs) * time.Second
			h := int(d.Hours())
			m := int(d.Minutes()) % 60
			return fmt.Sprintf("%dh %dm", h, m)
		},
		"statusColor": func(status string) string {
			switch status {
			case "active":
				return "#3fb950"
			case "error":
				return "#f85149"
			default:
				return "#8b949e"
			}
		},
		"aliveIcon": func(alive bool) string {
			if alive {
				return "🟢"
			}
			return "🔴"
		},
		"lower": strings.ToLower,
		"upper": strings.ToUpper,
		// json returns raw JSON as template.JS so html/template does NOT wrap it
		// in quotes. This lets `const x = {{json .Data}};` produce a JS object/array.
		"json": func(v interface{}) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"parseRule": func(rule string) []string {
			splitTarget := func(raw string) (string, string, string) {
				s := raw
				mapping := ""
				timeRange := ""
				for {
					atIdx := strings.LastIndex(s, "@")
					hashIdx := strings.LastIndex(s, "#")
					if atIdx < 0 && hashIdx < 0 {
						break
					}
					if atIdx > hashIdx {
						timeRange = s[atIdx+1:]
						s = s[:atIdx]
					} else {
						mapping = s[hashIdx+1:]
						s = s[:hashIdx]
					}
				}
				return s, mapping, timeRange
			}
			r := strings.TrimSpace(rule)
			r = strings.TrimPrefix(r, "//")
			parts := strings.Split(r, ",")
			if len(parts) == 1 {
				target, mapping, tr := splitTarget(parts[0])
				return []string{parts[0], "", target, mapping, tr}
			}
			if len(parts) == 2 {
				target, mapping, tr := splitTarget(parts[1])
				return []string{parts[0], "", target, mapping, tr}
			}
			keyword := parts[0]
			rawTarget := parts[len(parts)-1]
			value := strings.Join(parts[1:len(parts)-1], ",")
			target, mapping, tr := splitTarget(rawTarget)
			return []string{keyword, value, target, mapping, tr}
		},
		"sub": func(a, b int) int { return a - b },
		"add": func(a, b int) int { return a + b },
		"enabled": func(v *bool) bool {
			if v == nil {
				return true
			}
			return *v
		},
		"ruleEnabled": func(rule string) bool {
			return !strings.HasPrefix(strings.TrimSpace(rule), "//")
		},
		"host": func(addr string) string {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return addr
			}
			return host
		},
	}
}

// parseTemplates loads embedded HTML templates per-page.
func (s *AdminServer) parseTemplates() {
	funcs := templateFuncs()

	// Read layout content once
	layoutData, err := templates.ReadFile("templates/layout.html")
	if err != nil {
		panic(fmt.Sprintf("read layout.html fail: %v", err))
	}

	parsePage := func(name string) *template.Template {
		pageData, err := templates.ReadFile("templates/" + name)
		if err != nil {
			panic(fmt.Sprintf("read %s fail: %v", name, err))
		}
		// Combine: layout defines the base, page defines content block
		t := template.Must(template.New("").Funcs(funcs).Parse(string(layoutData)))
		t = template.Must(t.Parse(string(pageData)))
		return t
	}

	parseStandalone := func(name string) *template.Template {
		pageData, err := templates.ReadFile("templates/" + name)
		if err != nil {
			panic(fmt.Sprintf("read %s fail: %v", name, err))
		}
		return template.Must(template.New("").Funcs(funcs).Parse(string(pageData)))
	}

	s.pages = &pageTemplates{
		dashboard:     parsePage("dashboard.html"),
		tun:           parsePage("tun.html"),
		mesh:          parsePage("mesh.html"),
		logs:          parsePage("logs.html"),
		connections:   parsePage("connections.html"),
		proxies:       parsePage("proxies.html"),
		subscriptions: parsePage("subscriptions.html"),
		rules:         parsePage("rules.html"),
		mappings:      parsePage("mappings.html"),
		resolvers:     parsePage("resolvers.html"),
		reverseWizard: parsePage("reverse-wizard.html"),
		packagePage:   parsePage("package.html"),
		login:         parseStandalone("login.html"),
		setup:         parseStandalone("setup.html"),
	}
}

// Start launches the admin HTTP server.
func (s *AdminServer) Start() error {
	util.Logger.Printf("[INFO] [admin] Start() method entered, Enabled=%v", s.config.Enabled)
	if !s.config.Enabled {
		util.LogModuleInfo("admin", "disabled, skipping")
		return nil
	}
	util.Logger.Printf("[INFO] [admin] Start() continuing after Enabled check")

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.server = &http.Server{
		Handler: s.securityHeadersMiddleware(s.authMiddleware(mux)),
		// ReadTimeout covers the whole request including body: mesh package
		// pushes (7.9MB over slow links shared by concurrent pushes) can take
		// minutes, so keep read/write/client budgets aligned and generous.
		ReadTimeout:  300 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	util.Logger.Printf("[INFO] [admin] Start() reached TLS setup check")
	// Setup TLS if certificate and key are configured, or auto-generate if needed
	util.Logger.Printf("[INFO] [admin] TLS setup check: TLSCert=%q, TLSKey=%q, ClientCA=%q (len=%d)",
		s.config.TLSCert, s.config.TLSKey, 
		func() string {
			if len(s.config.ClientCA) > 50 {
				return s.config.ClientCA[:50] + "..."
			}
			return s.config.ClientCA
		}(),
		len(s.config.ClientCA))
	
	if s.config.TLSCert != "" && s.config.TLSKey != "" {
		var certPEM, keyPEM []byte
		var err error
		
		// Check if TLSCert is inline PEM or file path
		if strings.HasPrefix(s.config.TLSCert, "-----BEGIN") {
			certPEM = []byte(s.config.TLSCert)
			util.Logger.Printf("[INFO] [admin] TLS: using inline certificate")
		} else {
			certPEM, err = os.ReadFile(s.config.TLSCert)
			if err != nil {
				return fmt.Errorf("admin read TLS cert fail: %w", err)
			}
			util.Logger.Printf("[INFO] [admin] TLS: loaded certificate from file: %s", s.config.TLSCert)
		}
		
		// Check if TLSKey is inline PEM or file path
		if strings.HasPrefix(s.config.TLSKey, "-----BEGIN") {
			keyPEM = []byte(s.config.TLSKey)
			util.Logger.Printf("[INFO] [admin] TLS: using inline private key")
		} else {
			keyPEM, err = os.ReadFile(s.config.TLSKey)
			if err != nil {
				return fmt.Errorf("admin read TLS key fail: %w", err)
			}
			util.Logger.Printf("[INFO] [admin] TLS: loaded private key from file: %s", s.config.TLSKey)
		}
		
		util.Logger.Printf("[INFO] [admin] TLS: parsing X509 key pair (certPEM=%d bytes, keyPEM=%d bytes)", len(certPEM), len(keyPEM))
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			util.Logger.Printf("[ERROR] [admin] TLS: X509KeyPair failed: %v", err)
			return fmt.Errorf("admin parse TLS cert/key fail: %w", err)
		}
		util.Logger.Printf("[INFO] [admin] TLS: certificate parsed successfully")
		s.tlsConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
		}
		
		// Configure mTLS with embedded CA certificate
		embeddedCAConfigured := false
		util.LogModuleDebug("admin", "defaultClientCA length = %d", len(defaultClientCA))
		if len(defaultClientCA) > 0 {
			util.LogModuleDebug("admin", "entering mTLS configuration block")
			caCertPool := x509.NewCertPool()
			util.LogModuleDebug("admin", "created caCertPool, calling AppendCertsFromPEM")
			if !caCertPool.AppendCertsFromPEM(defaultClientCA) {
				util.Logger.Printf("[ERROR] [admin] TLS: failed to parse embedded CA certificate")
				return fmt.Errorf("admin parse embedded CA fail")
			}
			util.LogModuleDebug("admin", "AppendCertsFromPEM succeeded, setting ClientCAs")
			s.tlsConfig.ClientCAs = caCertPool
			embeddedCAConfigured = true
			util.LogModuleDebug("admin", "embeddedCAConfigured = true, checking AuthEnabled=%v", s.config.AuthEnabled)
			
			if s.config.AuthEnabled {
				s.tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
				util.LogModuleInfo("admin", "authentication enabled (mTLS or password) with embedded CA")
			} else {
				s.tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
				util.LogModuleInfo("admin", "authentication enabled (mTLS) with embedded CA")
			}
		}
		
		// Legacy support: if ClientCA is configured in database, use it (override embedded CA)
		if s.config.ClientCA != "" && !embeddedCAConfigured {
			var caCert []byte
			if strings.HasPrefix(s.config.ClientCA, "-----BEGIN CERTIFICATE-----") {
				caCert = []byte(s.config.ClientCA)
			} else {
				var err error
				caCert, err = os.ReadFile(s.config.ClientCA)
				if err != nil {
					return fmt.Errorf("admin read client CA fail: %w", err)
				}
			}
			caCertPool := x509.NewCertPool()
			if !caCertPool.AppendCertsFromPEM(caCert) {
				return fmt.Errorf("admin parse client CA fail")
			}
			s.tlsConfig.ClientCAs = caCertPool
			
			if s.config.AuthEnabled {
				s.tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
				util.LogModuleInfo("admin", "mTLS enabled with database CA (authentication enabled, mTLS or password)")
			} else {
				s.tlsConfig.ClientAuth = tls.NoClientCert
				util.LogModuleInfo("admin", "mTLS enabled with database CA (authentication disabled)")
			}
		}
		
		s.server.TLSConfig = s.tlsConfig
	} else if s.config.MeshOnly || s.config.Enabled {
		// Auto-generate TLS certificate if not configured
		// This ensures HTTPS always works for mesh access
		cert, key, err := s.loadOrGenerateTLSCert()
		if err != nil {
			util.LogModuleWarn("admin", "failed to auto-generate TLS cert: %v, falling back to HTTP", err)
		} else {
			tlsCert, err := tls.X509KeyPair(cert, key)
			if err != nil {
				util.LogModuleWarn("admin", "failed to parse auto-generated TLS cert: %v, falling back to HTTP", err)
			} else {
				s.tlsConfig = &tls.Config{
					Certificates: []tls.Certificate{tlsCert},
				}
				
				// Configure mTLS with embedded CA certificate (same as configured cert path)
				if len(defaultClientCA) > 0 {
					caCertPool := x509.NewCertPool()
					if caCertPool.AppendCertsFromPEM(defaultClientCA) {
						s.tlsConfig.ClientCAs = caCertPool
						s.tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven
						util.LogModuleInfo("admin", "auto-generated cert with mTLS (embedded CA)")
					}
				}
				
				s.server.TLSConfig = s.tlsConfig
				util.LogModuleInfo("admin", "auto-generated self-signed TLS certificate")
			}
		}
	}

	// Skip network listener if mesh-only mode
	if s.config.MeshOnly {
		util.LogModuleInfo("admin", "mesh-only mode enabled, no network listener")
		// Start the global version heartbeat
		util.DefaultVersionNotifier.StartHeartbeat(10 * time.Second)
		s.sseStopCh = make(chan struct{})
		s.sseWG.Add(1)
		go s.sseBroadcaster()
		return nil
	}

	addr := s.config.Addr
	if addr == "" {
		addr = "0.0.0.0:39999"
		util.LogModuleWarn("admin", "AdminConfig.Addr is empty, using default: %s", addr)
	}
	util.LogModuleInfo("admin", "starting network listener on %s", addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin listen on %s fail: %w", addr, err)
	}
	s.ln = ln

	// Use TLS if certificate and key are configured
	if s.tlsConfig != nil {
		util.LogModuleInfo("admin", "starting on https://%s", addr)
		go func() {
			// Wrap listener with TLS and use Serve (not ServeTLS)
			tlsLn := tls.NewListener(ln, s.tlsConfig)
			if err := s.server.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
				util.LogModuleError("admin", "serve error: %v", err)
			}
		}()
	} else {
		util.LogModuleInfo("admin", "starting on http://%s", addr)
		go func() {
			if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
				util.LogModuleError("admin", "serve error: %v", err)
			}
		}()
	}

	// Start the global version heartbeat. It is idempotent; multiple admin
	// server instances are not expected in this process.
	util.DefaultVersionNotifier.StartHeartbeat(10 * time.Second)

	s.sseStopCh = make(chan struct{})
	s.sseWG.Add(1)
	go s.sseBroadcaster()
	return nil
}

// Close shuts down the admin server.
func (s *AdminServer) Close() error {
	if s.sseStopCh != nil {
		close(s.sseStopCh)
	}
	if s.server != nil {
		if err := s.server.Close(); err != nil {
			return err
		}
	}
	if s.sseStopCh != nil {
		s.sseWG.Wait()
	}
	util.DefaultVersionNotifier.StopHeartbeat()
	return nil
}

// ServeConn serves a single connection using the admin server's handler.
// This allows bypassing the OS network stack for local connections (e.g., from mesh).
func (s *AdminServer) ServeConn(conn net.Conn) {
	if s.server == nil {
		util.LogModuleWarn("admin", "ServeConn called but server is nil")
		conn.Close()
		return
	}

	util.LogModuleInfo("admin", "ServeConn called from %s to %s, tlsConfig=%v", conn.RemoteAddr(), conn.LocalAddr(), s.tlsConfig != nil)

	// Wrap connection with TLS if configured (same as Start() does)
	var handledConn net.Conn
	if s.tlsConfig != nil {
		util.LogModuleInfo("admin", "Wrapping connection with TLS")
		handledConn = tls.Server(conn, s.tlsConfig)
	} else {
		handledConn = conn
	}

	ln := newSingleConnListener(handledConn)
	// Serve blocks until the connection is closed
	util.LogModuleInfo("admin", "Calling server.Serve")
	if err := s.server.Serve(ln); err != nil && err != io.EOF {
		util.LogModuleInfo("admin", "ServeConn ended: %v", err)
	}
	util.LogModuleInfo("admin", "ServeConn finished")
}

// singleConnListener is a net.Listener that yields a single connection then blocks until it's closed.
type singleConnListener struct {
	conn   net.Conn
	once   sync.Once
	doneCh chan struct{}
}

func newSingleConnListener(conn net.Conn) *singleConnListener {
	return &singleConnListener{
		conn:   conn,
		doneCh: make(chan struct{}),
	}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	var conn net.Conn
	l.once.Do(func() {
		conn = l.conn
	})
	if conn != nil {
		return conn, nil
	}
	// Block until the connection is closed
	<-l.doneCh
	return nil, io.EOF
}

func (l *singleConnListener) Close() error {
	select {
	case <-l.doneCh:
		// Already closed
	default:
		close(l.doneCh)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 39999}
}

// session constants
const sessionCookieName = "admin_session"
const sessionDuration = 7 * 24 * time.Hour

// signSession creates a signed session token.
func (s *AdminServer) signSession(username string, expires int64) string {
	secret := s.config.Token
	if secret == "" {
		secret = string(s.sessionSecret)
	}
	msg := fmt.Sprintf("%s:%d", username, expires)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(msg))
	return fmt.Sprintf("%s:%d:%s", username, expires, hex.EncodeToString(h.Sum(nil)))
}

// verifySession checks the session cookie and returns the username if valid.
func (s *AdminServer) verifySession(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	parts := strings.SplitN(c.Value, ":", 3)
	if len(parts) != 3 {
		return ""
	}
	username := parts[0]
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ""
	}
	if time.Now().Unix() > expires {
		return ""
	}
	expected := s.signSession(username, expires)
	if !hmac.Equal([]byte(c.Value), []byte(expected)) {
		return ""
	}
	return username
}

// isPublicPath returns true for paths that don't require authentication.
func isPublicPath(path string, method string) bool {
	// Static files and auth endpoints
	if path == "/login" || path == "/api/login" || path == "/api/captcha" || path == "/setup" || path == "/api/setup" ||
		strings.HasPrefix(path, "/static/") {
		return true
	}

	// Mesh package distribution endpoints (read-only + receive)
	if method == http.MethodGet {
		// GET /api/packages - list packages
		if path == "/api/packages" {
			return true
		}
		// GET /api/packages/{id}/download - download package
		if strings.HasPrefix(path, "/api/packages/") && strings.HasSuffix(path, "/download") {
			return true
		}
	}
	// POST /api/packages/receive - receive package from mesh peer
	if method == http.MethodPost && path == "/api/packages/receive" {
		return true
	}

	return false
}

// authMiddleware checks authentication (session or token).
func (s *AdminServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public paths are always accessible
		if isPublicPath(r.URL.Path, r.Method) {
			next.ServeHTTP(w, r)
			return
		}

		// Authentication is enforced only if AuthEnabled flag is true in config.
		// The embedded CA certificate (defaultClientCA) is used for mTLS when
		// AuthEnabled is true, but does not by itself require authentication.
		authRequired := s.config.AuthEnabled
		
		// If auth is not required, allow everything
		if !authRequired {
			next.ServeHTTP(w, r)
			return
		}

		// Authentication required: try multiple auth methods (user chooses)

		// 1. Try mTLS client certificate authentication
		// Valid CN values: "all" (global cert) or current mesh nodeID (node cert)
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			cn := r.TLS.PeerCertificates[0].Subject.CommonName
			
			// Get current mesh nodeID for node certificate validation
			var nodeID string
			if mesh, err := db.GetMeshConfig(); err == nil && mesh != nil {
				nodeID = mesh.NodeID
			}
			
			// Accept if CN is "all" (global) or matches current nodeID
			if cn == "all" || cn == nodeID {
				certType := "global"
				if cn == nodeID {
					certType = "node"
				}
				util.LogModuleInfo("admin", "mTLS auth success: %s cert, cn=%s", certType, cn)
				next.ServeHTTP(w, r)
				return
			}
			// CN not valid, reject
			util.LogModuleWarn("admin", "mTLS auth failed: invalid cn=%s (expected 'all' or nodeID=%s)", cn, nodeID)
		}

		// 2. Try session-based login
		if user := s.verifySession(r); user != "" {
			next.ServeHTTP(w, r)
			return
		}

		// 3. Try signature-based auth (public key verification)
		if s.config.PublicKey != "" && s.verifySignature(r) {
			next.ServeHTTP(w, r)
			return
		}

		// 4. Try token-based auth (for API/programmatic access)
		token := r.URL.Query().Get("token")
		if token == "" {
			token = r.Header.Get("X-Admin-Token")
		}
		if token == "" {
			if c, err := r.Cookie("admin_token"); err == nil {
				token = c.Value
			}
		}
		if token != "" && token == s.config.Token {
			next.ServeHTTP(w, r)
			return
		}

		// 5. Not authenticated — redirect to login for pages, 401 for API
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		// Use a relative Location header so browsers resolve it against the
		// address bar path. This keeps sub-path nginx deployments working
		// without requiring X-Forwarded-Prefix support.
		w.Header().Set("Location", "./login")
		w.WriteHeader(http.StatusFound)
	})
}

// verifySignature checks if the request has a valid signature using the configured public key.
// Signature format: X-Signature header contains base64-encoded signature of (timestamp + "\n" + method + "\n" + path + "\n" + bodyHash)
// X-Timestamp header contains Unix timestamp (used to prevent replay attacks, valid for 5 minutes)
func (s *AdminServer) verifySignature(r *http.Request) bool {
	sigHeader := r.Header.Get("X-Signature")
	tsHeader := r.Header.Get("X-Timestamp")
	if sigHeader == "" || tsHeader == "" {
		return false
	}

	// Parse and validate timestamp (prevent replay attacks, 5-minute window)
	ts, err := strconv.ParseInt(tsHeader, 10, 64)
	if err != nil {
		return false
	}
	now := time.Now().Unix()
	if abs(now-ts) > 300 { // 5 minutes
		return false
	}

	// Decode signature
	sig, err := hex.DecodeString(sigHeader)
	if err != nil {
		// Try base64 if hex fails
		sig, err = decodeBase64(sigHeader)
		if err != nil {
			return false
		}
	}

	// Read body and compute hash
	var bodyBytes []byte
	if r.Body != nil {
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			return false
		}
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // Restore body for handlers
	}
	bodyHash := sha256.Sum256(bodyBytes)

	// Construct signed data: timestamp + "\n" + method + "\n" + path + "\n" + bodyHash
	signedData := fmt.Sprintf("%s\n%s\n%s\n%s",
		tsHeader,
		r.Method,
		r.URL.Path,
		hex.EncodeToString(bodyHash[:]),
	)

	// Parse public key
	block, _ := pem.Decode([]byte(s.config.PublicKey))
	if block == nil {
		return false
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return false
	}

	// Verify signature based on key type
	hash := sha256.Sum256([]byte(signedData))
	switch pubKey := pub.(type) {
	case *rsa.PublicKey:
		return rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, hash[:], sig) == nil
	case *ecdsa.PublicKey:
		return ecdsa.VerifyASN1(pubKey, hash[:], sig)
	default:
		return false
	}
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

func decodeBase64(s string) ([]byte, error) {
	// Try with padding
	s = strings.TrimRight(s, "=")
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	switch len(s) % 4 {
	case 2:
		s += "=="
	case 3:
		s += "="
	}
	return base64.StdEncoding.DecodeString(s)
}

// securityHeadersMiddleware adds Content-Security-Policy and other security
// headers to every admin response. The CSP allows only same-origin scripts,
// styles, and API calls so that externally injected resources (e.g. ISP
// captive-portal scripts) cannot block or pollute the admin UI.
func (s *AdminServer) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'unsafe-inline'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"connect-src 'self'; "+
				"font-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self';")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// registerRoutes sets up all HTTP routes.
func (s *AdminServer) registerRoutes(mux *http.ServeMux) {
	// Static files — serve from embedded static/ directory.
	// The embed paths are "static/style.css" etc., so we strip the /static prefix.
	staticFS, _ := fs.Sub(static, "static")
	fileServer := http.FileServer(http.FS(staticFS))
	mux.Handle("/static/", http.StripPrefix("/static", fileServer))

	// Pages
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/tun", s.handleTUNPage)
	mux.HandleFunc("/mesh", s.handleMeshPage)
	mux.HandleFunc("/proxies", s.handleProxiesPage)
	mux.HandleFunc("/subscriptions", s.handleSubscriptionsPage)
	mux.HandleFunc("/rules", s.handleRulesPage)
	mux.HandleFunc("/mappings", s.handleMappingsPage)
	mux.HandleFunc("/resolvers", s.handleResolversPage)
	mux.HandleFunc("/reverse", s.handleReverseWizardPage)
	mux.HandleFunc("/logs", s.handleLogsPage)
	mux.HandleFunc("/connections", s.handleConnectionsPage)
	mux.HandleFunc("/admin", s.handleAdminPage)
	mux.HandleFunc("/package", s.handlePackagePage)
	mux.HandleFunc("/login", s.handleLoginPage)
	mux.HandleFunc("/setup", s.handleSetupPage)

	// API endpoints — use HandleFunc with path checks for RESTful routing
	mux.HandleFunc("/api/stats", s.apiStats)
	mux.HandleFunc("/api/config/mesh", s.apiConfigMesh)
	mux.HandleFunc("/api/config/admin", s.apiConfigAdmin)
	mux.HandleFunc("/api/config/reload", s.apiReload)
	mux.HandleFunc("/api/config/target", s.apiTarget)
	mux.HandleFunc("/api/proxies", s.apiProxies)
	mux.HandleFunc("/api/proxies/health-check/", s.apiProxyHealthCheck)
	mux.HandleFunc("/api/proxies/", s.apiProxies)
	mux.HandleFunc("/api/rules", s.apiRules)
	mux.HandleFunc("/api/rules/", s.apiRules)
	mux.HandleFunc("/api/mappings", s.apiMappings)
	mux.HandleFunc("/api/mappings/", s.apiMappings)
	mux.HandleFunc("/api/resolvers", s.apiResolvers)
	mux.HandleFunc("/api/resolvers/", s.apiResolvers)
	mux.HandleFunc("/api/subscriptions", s.apiSubscriptions)
	mux.HandleFunc("/api/subscriptions/", s.apiSubscriptionActions)
	mux.HandleFunc("/api/groups", s.apiGroups)
	mux.HandleFunc("/api/groups/", s.apiGroupActions)
	mux.HandleFunc("/api/health", s.apiHealth)
	mux.HandleFunc("/api/connections", s.apiConnections)
	mux.HandleFunc("/api/activeconns", s.apiActiveConns)
	mux.HandleFunc("/api/reverse", s.apiReverse)
	mux.HandleFunc("/api/reverse/bindings", s.apiReverseBindings)
	mux.HandleFunc("/api/reverse/bindings/", s.apiReverseBindings)
	mux.HandleFunc("/api/reverse/", s.apiReverseItem)
	mux.HandleFunc("/api/p2p", s.apiP2P)
	mux.HandleFunc("/api/packages", s.apiPackages)
	mux.HandleFunc("/api/packages/upload", s.apiPackageUpload)
	mux.HandleFunc("/api/packages/receive", s.apiPackageReceive)
	mux.HandleFunc("/api/packages/", s.apiPackageItem)
	mux.HandleFunc("/api/mesh", s.apiMesh)
	mux.HandleFunc("/api/mesh/config", s.apiMesh)
	mux.HandleFunc("/api/mesh/gossip", s.apiMesh)
	mux.HandleFunc("/api/mesh/topology", s.apiMesh)
	mux.HandleFunc("/api/mesh/domain-suffixes", s.apiMeshDomainSuffixes)
	mux.HandleFunc("/api/mesh/domain-suffixes/", s.apiMeshDomainSuffixItem)
	mux.HandleFunc("/api/mesh/advertise", s.apiMeshAdvertise)
	mux.HandleFunc("/api/mesh/advertise/", s.apiMeshAdvertiseItem)
	mux.HandleFunc("/api/tun", s.apiTUN)
	mux.HandleFunc("/api/events", s.apiEvents)
	mux.HandleFunc("/api/versions", s.apiVersions)
	mux.HandleFunc("/api/login", s.apiLogin)
	mux.HandleFunc("/api/captcha", s.apiCaptcha)
	mux.HandleFunc("/api/logout", s.apiLogout)
	mux.HandleFunc("/api/me", s.apiMe)
	mux.HandleFunc("/api/admin/auth", s.apiAdminAuth)
	mux.HandleFunc("/api/setup", s.apiSetup)
	mux.HandleFunc("/api/restart", s.apiRestart)
	mux.HandleFunc("/api/log/modules", s.apiLogModules)
}

// ========== Page Handlers ==========

func (s *AdminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Runtime stats from database
	s.stats.CollectFromConfig()

	// Counts from the database
	proxies, _ := db.ListProxies()
	rules, _ := db.ListRules()
	mappings, _ := db.ListMappings()
	groups, _ := db.ListProxyGroups()
	subs, _ := db.ListSubscriptions()

	// Get version from GetCurrentVersion if available, otherwise fallback to env
	version := ""
	if s.GetCurrentVersion != nil {
		version = s.GetCurrentVersion()
	}
	if version == "" {
		version = os.Getenv("PHAETHON_VERSION")
	}

	data := map[string]interface{}{
		"Title":             "Dashboard",
		"Version":           version,
		"Stats":             s.stats.GetSnapshot(),
		"UptimeSeconds":     int64(time.Since(s.stats.StartTime()).Seconds()),
		"ProxyCount":        len(proxies),
		"RuleCount":         len(rules),
		"MappingCount":      len(mappings),
		"GroupCount":        len(groups),
		"SubscriptionCount": len(subs),
		"EnvInfo":           s.envInfo(),
		"SaveTarget":        "base",
		"CanSwitch":         false,
		"TUNAvailable":      tun.Available(),
		"MeshEnabled":       mesh.GlobalMeshManager != nil,
	}
	s.render(w, r, "dashboard.html", data)
}

func (s *AdminServer) handleTUNPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"Title":        "TUN",
		"Version":      os.Getenv("PHAETHON_VERSION"),
		"TUNAvailable": tun.Available(),
	}
	s.render(w, r, "tun.html", data)
}

func (s *AdminServer) handleMeshPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"Title":       "Mesh",
		"Version":     os.Getenv("PHAETHON_VERSION"),
		"MeshEnabled": mesh.GlobalMeshManager != nil,
	}
	s.render(w, r, "mesh.html", data)
}

func (s *AdminServer) handlePackagePage(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"Title":   "Package",
		"Version": os.Getenv("PHAETHON_VERSION"),
	}
	s.render(w, r, "package.html", data)
}

// envInfo detects whether an environment-specific config override file exists.
func (s *AdminServer) envInfo() map[string]interface{} {
	env := os.Getenv("ENV_NAME")
	if env == "" {
		env = util.JavaProp("env.name")
	}
	info := map[string]interface{}{
		"Name":       env,
		"HasOverlay": false,
	}
	return info
}

func (s *AdminServer) handleProxiesPage(w http.ResponseWriter, r *http.Request) {
	proxies, _ := db.ListProxies()
	groups, _ := db.ListProxyGroups()
	subs, _ := db.ListSubscriptions()

	// Build group stats from database (single source of truth)
	groupStats := make(map[string]map[string]interface{})
	for _, g := range groups {
		active := g.PickActiveMember()
		activeName := ""
		if active.Name != "" {
			activeName = active.Name
		}
		groupStats[g.Name] = map[string]interface{}{
			"total":  len(g.GetMembers()),
			"active": activeName,
		}
	}
	displayGroups := groups

	// Build proxy summaries for JavaScript (includes IsP2P/IsUDP computed values)
	proxySummaries := make([]map[string]interface{}, len(proxies))
	for i, p := range proxies {
		proxySummaries[i] = proxySummary(p)
	}

	data := map[string]interface{}{
		"Title":         "Proxies",
		"Proxies":       proxies,
		"ProxySummaries": proxySummaries,
		"Groups":        displayGroups,
		"GroupStats":    groupStats,
		"Subscriptions": subs,
		"SaveTarget":    "base",
		"CanSwitch":     false,
	}
	s.render(w, r, "proxies.html", data)
}

func (s *AdminServer) handleSubscriptionsPage(w http.ResponseWriter, r *http.Request) {
	subs, _ := db.ListSubscriptions()

	// Build subscription snapshot from database
	subData := make(map[string]map[string]interface{})
	for _, sub := range subs {
		proxies := s.getSubscriptionProxies(sub.Name)
		nodes := make([]map[string]interface{}, 0, len(proxies))
		for name, p := range proxies {
			nodes = append(nodes, map[string]interface{}{
				"name":   name,
				"type":   p.Type,
				"server": p.Server,
				"port":   p.Port,
			})
		}
		subData[sub.Name] = map[string]interface{}{
			"nodes": nodes,
			"url":   sub.URL,
		}
	}

	data := map[string]interface{}{
		"Title":         "Subscriptions",
		"Subscriptions": subs,
		"SubData":       subData,
		"SaveTarget":    "base",
		"CanSwitch":     false,
	}
	s.render(w, r, "subscriptions.html", data)
}

func (s *AdminServer) handleRulesPage(w http.ResponseWriter, r *http.Request) {
	rules, _ := db.ListRules()
	proxies, _ := db.ListProxies()
	groups, _ := db.ListProxyGroups()
	mappings, _ := db.ListMappings()
	data := map[string]interface{}{
		"Title":      "Rules",
		"Rules":      rules,
		"Proxies":    proxies,
		"Groups":     groups,
		"Mappings":   mappings,
		"SaveTarget": "base",
		"CanSwitch":  false,
	}
	s.render(w, r, "rules.html", data)
}

func (s *AdminServer) handleMappingsPage(w http.ResponseWriter, r *http.Request) {
	mappings, _ := db.ListMappings()
	data := map[string]interface{}{
		"Title":      "Mappings",
		"Mappings":   mappings,
		"SaveTarget": "base",
		"CanSwitch":  false,
	}
	s.render(w, r, "mappings.html", data)
}

func (s *AdminServer) handleResolversPage(w http.ResponseWriter, r *http.Request) {
	resolvers, _ := db.ListResolvers()
	data := map[string]interface{}{
		"Title":     "Resolvers",
		"Resolvers": resolvers,
	}
	s.render(w, r, "resolvers.html", data)
}

func (s *AdminServer) handleReverseWizardPage(w http.ResponseWriter, r *http.Request) {
	proxies, _ := db.ListProxies()
	// Load instance ReverseID from file
	dataDir := filepath.Join("data", "setup")
	instanceReverseID, _ := reverse.GetReverseID(dataDir)

	// Build proxy list with reverse-support flag
	type proxyInfo struct {
		*config.Proxy
		SupportsReverse bool
	}
	var proxyInfos []proxyInfo
	for _, p := range proxies {
		supports := false
		switch p.Type {
		case "socks5", "trojan", "h_tunnel":
			supports = true
		}
		proxyInfos = append(proxyInfos, proxyInfo{p, supports})
	}

	data := map[string]interface{}{
		"Title":             "Reverse",
		"Proxies":           proxyInfos,
		"ReverseConfigs":    s.currentReverseConfigs(),
		"InstanceReverseID": instanceReverseID,
		"SaveTarget":        "base",
		"CanSwitch":         false,
	}
	s.render(w, r, "reverse-wizard.html", data)
}

func (s *AdminServer) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"Title":   "Logs",
		"Version": os.Getenv("PHAETHON_VERSION"),
	}
	s.render(w, r, "logs.html", data)
}

func (s *AdminServer) handleConnectionsPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"Title":   "Connections",
		"Version": os.Getenv("PHAETHON_VERSION"),
	}
	s.render(w, r, "connections.html", data)
}

func (s *AdminServer) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages.login.Execute(w, map[string]interface{}{
		"AuthEnabled": s.config.AuthEnabled,
	}); err != nil {
		util.LogModuleError("admin", "login template error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages.setup.Execute(w, map[string]interface{}{
		"AuthEnabled": s.config.AuthEnabled,
		"Username":    s.config.Username,
	}); err != nil {
		util.LogModuleError("admin", "setup template error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"Title": "Admin Settings",
	}
	s.render(w, r, "admin.html", data)
}

// ========== API Handlers ==========

func (s *AdminServer) apiStats(w http.ResponseWriter, r *http.Request) {
	s.stats.CollectFromConfig()
	jsonResponse(w, s.stats.GetSnapshot())
}

func (s *AdminServer) apiConnections(w http.ResponseWriter, r *http.Request) {
	var logs []connlog.Event
	afterStr := r.URL.Query().Get("after")
	if afterStr != "" {
		if after, err := strconv.ParseUint(afterStr, 10, 64); err == nil {
			logs = connlog.GetLogsAfterSeq(after)
		} else {
			logs = connlog.GetLogs()
		}
	} else {
		logs = connlog.GetLogs()
	}
	version := connlog.GetVersion()
	jsonResponse(w, map[string]interface{}{
		"version": version,
		"logs":    logs,
	})
}

func (s *AdminServer) apiActiveConns(w http.ResponseWriter, r *http.Request) {
	afterStr := r.URL.Query().Get("after")
	version := connlog.GetVersion()

	if afterStr != "" {
		after, err := strconv.ParseUint(afterStr, 10, 64)
		if err == nil {
			entries, conns, stale := connlog.GetActiveConnsAfterSeq(after)
			if stale {
				jsonResponse(w, map[string]interface{}{
					"version":     version,
					"stale":       true,
					"connections": conns,
				})
				return
			}
			jsonResponse(w, map[string]interface{}{
				"version": version,
				"stale":   false,
				"journal": entries,
			})
			return
		}
	}

	conns := connlog.GetActiveConns()
	jsonResponse(w, map[string]interface{}{
		"version":     version,
		"stale":       true,
		"connections": conns,
	})
}

// apiEvents streams server-sent events for real-time status updates.
// Each connection is registered with the global VersionNotifier, which sends
// an initial heartbeat and then re-broadcasts the full version vector as a
// heartbeat whenever BumpVersion is called. A periodic heartbeat also keeps
// the connection alive and lets clients recover silently after transient
// disconnects. If SSE is unavailable, clients can fall back to /api/versions.
func (s *AdminServer) apiEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Disable the server-level WriteTimeout for this long-lived SSE stream.
	if rc := http.NewResponseController(w); rc != nil {
		rc.SetWriteDeadline(time.Time{})
	}

	writer := util.NewSSEWriter(w)
	if writer == nil {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	util.DefaultVersionNotifier.Subscribe(writer)
	defer util.DefaultVersionNotifier.Unsubscribe(writer)

	// Wait for the client to disconnect.
	<-r.Context().Done()
}

// apiVersions returns the current full version vector for polling clients.
// It is the REST fallback used when the SSE stream is unavailable.
func (s *AdminServer) apiVersions(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, util.DefaultVersionNotifier.Versions())
}

// reverseEventPayload builds the data payload for a "reverse" SSE event from
// the current runtime reverse configuration, preferring the persisted setup
// profile for fields that are only updated there (assigned port / last error).
// reverseEventPayload builds the data payload for a "reverse" SSE event from
// the current runtime reverse configurations. It returns an array so the UI can
// display multiple reverse connections in a single process.
func (s *AdminServer) reverseEventPayload(configs []*config.ReverseConfig) interface{} {
	items := make([]map[string]interface{}, 0, len(configs))
	for _, rc := range configs {
		if rc == nil {
			continue
		}
		items = append(items, map[string]interface{}{
			"name":          rc.Name,
			"seq":           rc.Seq,
			"enabled":       rc.Enabled,
			"registryProxy": rc.RegistryProxy,
			"listenerProto": rc.ListenerProto,
			"assignedPort":  rc.AssignedPort,
			"lastError":     rc.LastError,
			"targetAddress": rc.TargetAddress,
		})
	}
	return items
}

// sseBroadcaster periodically snapshots stats for SSE broadcasting.
// Version bumping is event-driven (OnConnect, OnDisconnect, UpdateHealth).
func (s *AdminServer) sseBroadcaster() {
	defer s.sseWG.Done()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.stats.CollectFromConfig()
		case <-s.sseStopCh:
			return
		}
	}
}

// apiConfigMesh handles GET/PUT for mesh foundational settings.
// PUT triggers OnReload (mesh subsystem restart: re-announce, rebuild routing).
func (s *AdminServer) apiConfigMesh(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		meshCfg, err := db.GetMeshConfig()
		if err != nil || meshCfg == nil {
			jsonResponse(w, map[string]interface{}{})
			return
		}
		jsonResponse(w, meshCfg)

	case http.MethodPut:
		// Dangerous operation: require password confirmation via header
		password := r.Header.Get("X-Password-Confirm")
		if password == "" {
			httpError(w, "password confirmation required (X-Password-Confirm header)", http.StatusBadRequest)
			return
		}
		if !s.config.AuthEnabled {
			httpError(w, "this operation requires authentication to be enabled", http.StatusForbidden)
			return
		}
		if password != s.config.Password {
			httpError(w, "incorrect password", http.StatusForbidden)
			return
		}

		var updates map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			httpError(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		meshCfg, _ := db.GetMeshConfig()
		if meshCfg == nil {
			meshCfg = &config.MeshConfig{}
		}

		// Merge updates: only update fields that are present
		if nodeID, ok := updates["NodeID"].(string); ok && nodeID != "" {
			meshCfg.NodeID = nodeID
		}
		if subnet, ok := updates["Subnet"].(string); ok {
			meshCfg.Subnet = subnet
		}
		if network, ok := updates["Network"].(string); ok {
			meshCfg.Network = network
		}
		if domainSuffixes, ok := updates["DomainSuffixes"].([]interface{}); ok {
			suffixes := make([]string, 0, len(domainSuffixes))
			for _, s := range domainSuffixes {
				if str, ok := s.(string); ok {
					suffixes = append(suffixes, str)
				}
			}
			meshCfg.DomainSuffixes = suffixes
		}
		if advertise, ok := updates["Advertise"].([]interface{}); ok {
			advList := make([]string, 0, len(advertise))
			for _, a := range advertise {
				if str, ok := a.(string); ok {
					advList = append(advList, str)
				}
			}
			meshCfg.Advertise = advList
		}
		if forwardingTrace, ok := updates["ForwardingTrace"].(bool); ok {
			meshCfg.ForwardingTrace = forwardingTrace
		}

		// Validate required fields
		if meshCfg.NodeID == "" {
			httpError(w, "node-id is required", http.StatusBadRequest)
			return
		}

		if err := db.PutMeshConfig(meshCfg); err != nil {
			httpError(w, "persist mesh config fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("config")

		if s.OnReload != nil {
			go s.OnReload()
		}

		util.LogModuleInfo("admin", "mesh config updated (node-id=%s, persisted to database)", meshCfg.NodeID)
		jsonResponse(w, map[string]string{"status": "ok"})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiConfigAdmin handles GET/PUT for admin foundational settings.
// PUT triggers OnReload (admin server restart: rebind port, update auth).
func (s *AdminServer) apiConfigAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonResponse(w, s.config)

	case http.MethodPut:
		// Dangerous operation: require password confirmation via header
		password := r.Header.Get("X-Password-Confirm")
		if password == "" {
			httpError(w, "password confirmation required (X-Password-Confirm header)", http.StatusBadRequest)
			return
		}
		if !s.config.AuthEnabled {
			httpError(w, "this operation requires authentication to be enabled", http.StatusForbidden)
			return
		}
		if password != s.config.Password {
			httpError(w, "incorrect password", http.StatusForbidden)
			return
		}

		var updates map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
			httpError(w, "invalid json: "+err.Error(), http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		// Merge updates: only update fields that are present
		if addr, ok := updates["Addr"].(string); ok && addr != "" {
			s.config.Addr = addr
		}
		if authEnabled, ok := updates["AuthEnabled"].(bool); ok {
			oldAuthEnabled := s.config.AuthEnabled
			// Hot-reload: update runtime auth settings immediately
			if authEnabled != oldAuthEnabled {
				s.config.AuthEnabled = authEnabled
				util.LogModuleInfo("admin", "auth-enabled changed to %v, hot-reloaded", authEnabled)
			}
		}
		if username, ok := updates["Username"].(string); ok {
			s.config.Username = username
		}
		if password, ok := updates["Password"].(string); ok && password != "" {
			s.config.Password = password
		}
		if token, ok := updates["Token"].(string); ok {
			s.config.Token = token
		}
		if publicKey, ok := updates["PublicKey"].(string); ok {
			s.config.PublicKey = publicKey
		}
		if tlsCert, ok := updates["TLSCert"].(string); ok {
			s.config.TLSCert = tlsCert
		}
		if tlsKey, ok := updates["TLSKey"].(string); ok {
			s.config.TLSKey = tlsKey
		}
		if clientCA, ok := updates["ClientCA"].(string); ok {
			s.config.ClientCA = clientCA
		}
		if meshOnly, ok := updates["MeshOnly"].(bool); ok {
			s.config.MeshOnly = meshOnly
		}
		if pkgMaxMB, ok := updates["PackageUploadMaxMB"].(float64); ok {
			s.config.PackageUploadMaxMB = int(pkgMaxMB)
		}

		// Validate required fields
		if s.config.Addr == "" {
			httpError(w, "addr is required", http.StatusBadRequest)
			return
		}

		if err := db.PutAdminConfig(s.config); err != nil {
			httpError(w, "persist admin config fail: "+err.Error(), http.StatusInternalServerError)
			return
		}

		util.DefaultVersionNotifier.BumpVersion("config")

		if s.OnReload != nil {
			go s.OnReload()
		}

		util.LogModuleInfo("admin", "admin config updated (addr=%s, persisted to database)", s.config.Addr)
		jsonResponse(w, map[string]string{"status": "ok"})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiProxyHealthCheck runs a one-off connectivity check for a top-level proxy.
func (s *AdminServer) apiProxyHealthCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/proxies/health-check/")
	if name == "" {
		httpError(w, "proxy name required", http.StatusBadRequest)
		return
	}
	if s.CheckProxyHealth == nil {
		httpError(w, "proxy health check not configured", http.StatusServiceUnavailable)
		return
	}
	info, err := s.CheckProxyHealth(name)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"name":      name,
		"alive":     info.Alive,
		"latencyMs": info.Latency.Milliseconds(),
		"lastCheck": info.LastCheck,
	})
}

func (s *AdminServer) apiReload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Trigger a full config reload from disk.
	// This recreates listeners, reverse bindings, and other runtime resources.
	if s.OnReload != nil {
		s.OnReload()
	}

	util.LogModuleInfo("admin", "reload triggered via API")
	jsonResponse(w, map[string]string{"status": "reload triggered"})
}

func (s *AdminServer) apiRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Dangerous operation: require password confirmation via header
	password := r.Header.Get("X-Password-Confirm")
	if password == "" {
		httpError(w, "password confirmation required (X-Password-Confirm header)", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	authEnabled := s.config.AuthEnabled
	storedPassword := s.config.Password
	s.mu.RUnlock()
	if !authEnabled {
		httpError(w, "this operation requires authentication to be enabled", http.StatusForbidden)
		return
	}
	if password != storedPassword {
		httpError(w, "incorrect password", http.StatusForbidden)
		return
	}

	util.LogModuleInfo("admin", "restart requested via API")
	jsonResponse(w, map[string]string{"status": "restarting"})

	// Trigger restart after response is sent
	if s.OnRestart != nil {
		go func() {
			time.Sleep(500 * time.Millisecond)
			s.OnRestart()
		}()
	}
}

func (s *AdminServer) apiTarget(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		info := map[string]interface{}{
			"saveTarget": "base",
			"basePath":   "",
			"canSwitch":  false,
		}
		jsonResponse(w, info)

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// proxyReference describes one place where a proxy is still referenced.
type proxyReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// findProxyReferences returns every rule, group, proxy, or mapping that refers
// to the named proxy. It queries the database directly.
// findProxyReferences queries the database directly for cross-reference checks.
func (s *AdminServer) findProxyReferences(name string) ([]proxyReference, error) {
	var refs []proxyReference
	if name == "" {
		return refs, nil
	}

	// Query rules directly from database
	rules, err := db.ListRules()
	if err != nil {
		return nil, err
	}

	for _, rule := range rules {
		rs := strings.SplitN(rule, ",", 3)
		var target string
		switch {
		case len(rs) >= 3 && (rs[0] == "DOMAIN-SUFFIX" || rs[0] == "IP-CIDR"):
			target = strings.Trim(rs[2], `"'`)
		case len(rs) >= 2 && rs[0] == "MATCH":
			target = strings.Trim(rs[1], `"'`)
		}
		if target == name {
			refs = append(refs, proxyReference{Kind: "rule", Name: rule})
		}
	}

	// Query proxy groups directly from database
	groups, err := db.ListProxyGroups()
	if err != nil {
		return nil, err
	}

	for _, g := range groups {
		if g == nil {
			continue
		}
		for _, m := range g.ManualProxies {
			if m == name {
				refs = append(refs, proxyReference{Kind: "group", Name: g.Name})
				break
			}
		}
	}

	// Query proxies directly from database
	proxies, err := db.ListProxies()
	if err != nil {
		return nil, err
	}

	for _, p := range proxies {
		if p != nil && p.ViaProxy == name {
			refs = append(refs, proxyReference{Kind: "proxy", Name: p.Name})
		}
	}

	// Query mappings directly from database
	mappings, err := db.ListMappings()
	if err != nil {
		return nil, err
	}

	for _, m := range mappings {
		if m != nil && m.ReverseProxy == name {
			refs = append(refs, proxyReference{Kind: "mapping", Name: m.Name})
		}
	}

	// Query reverse configs directly from database
	reverseConfigs, err := db.ListReverseConfigs()
	if err != nil {
		return nil, err
	}

	for _, rc := range reverseConfigs {
		if rc != nil && rc.RegistryProxy == name {
			refs = append(refs, proxyReference{Kind: "reverse", Name: rc.Name})
		}
	}

	return refs, nil
}

// findGroupReferences returns every rule, group, proxy, mapping, or reverse
// config that refers to the named proxy group. It queries the database directly.
func (s *AdminServer) findGroupReferences(name string) ([]proxyReference, error) {
	var refs []proxyReference
	if name == "" {
		return refs, nil
	}

	// Query rules directly from database
	rules, err := db.ListRules()
	if err != nil {
		return nil, err
	}

	for _, rule := range rules {
		rs := strings.SplitN(rule, ",", 3)
		var target string
		switch {
		case len(rs) >= 3 && (rs[0] == "DOMAIN-SUFFIX" || rs[0] == "IP-CIDR"):
			target = strings.Trim(rs[2], `"'`)
		case len(rs) >= 2 && rs[0] == "MATCH":
			target = strings.Trim(rs[1], `"'`)
		}
		if target == name {
			refs = append(refs, proxyReference{Kind: "rule", Name: rule})
		}
	}

	// Query proxy groups directly from database
	groups, err := db.ListProxyGroups()
	if err != nil {
		return nil, err
	}

	for _, g := range groups {
		if g == nil || g.Name == name {
			continue
		}
		for _, m := range g.ManualProxies {
			if m == name {
				refs = append(refs, proxyReference{Kind: "group", Name: g.Name})
				break
			}
		}
	}

	// Query proxies directly from database
	proxies, err := db.ListProxies()
	if err != nil {
		return nil, err
	}

	for _, p := range proxies {
		if p != nil && p.ViaProxy == name {
			refs = append(refs, proxyReference{Kind: "proxy", Name: p.Name})
		}
	}

	// Query mappings directly from database
	mappings, err := db.ListMappings()
	if err != nil {
		return nil, err
	}

	for _, m := range mappings {
		if m != nil && m.ReverseProxy == name {
			refs = append(refs, proxyReference{Kind: "mapping", Name: m.Name})
		}
	}

	// Query reverse configs directly from database
	reverseConfigs, err := db.ListReverseConfigs()
	if err != nil {
		return nil, err
	}

	for _, rc := range reverseConfigs {
		if rc != nil && rc.RegistryProxy == name {
			refs = append(refs, proxyReference{Kind: "reverse", Name: rc.Name})
		}
	}

	return refs, nil
}

func (s *AdminServer) apiProxies(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		// Check if this is a P2P toggle request
		if strings.HasSuffix(r.URL.Path, "/p2p") {
			s.apiToggleP2P(w, r)
			return
		}
		s.apiToggleProxy(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		proxies, err := db.ListProxies()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		summaries := make([]map[string]interface{}, 0, len(proxies))
		for _, p := range proxies {
			summaries = append(summaries, proxySummary(p))
		}
		jsonResponse(w, summaries)

	case http.MethodPost:
		var p config.Proxy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		if p.Name == "" || p.Type == "" {
			httpError(w, "name and type required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		// Check if replacing an existing proxy
		existing, _ := db.GetProxy(p.Name)
		replaced := existing != nil

		// Direct database write
		if err := db.PutProxy(&p); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogModuleWarn("admin", "incremental update after proxy add failed: %v", err)
			}
		}
		util.DefaultVersionNotifier.BumpVersion("proxies")
		s.mu.Unlock()
		if replaced {
			util.LogModuleInfo("admin", "proxy updated: %s (%s)", p.Name, p.Type)
			jsonResponse(w, proxySummary(&p))
		} else {
			util.LogModuleInfo("admin", "proxy added: %s (%s)", p.Name, p.Type)
			w.WriteHeader(http.StatusCreated)
			jsonResponse(w, proxySummary(&p))
		}

	case http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/api/proxies/")
		if name == "" {
			httpError(w, "proxy name required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		// Block deletion while the proxy is still referenced.
		refs, err := s.findProxyReferences(name)
		if err != nil {
			s.mu.Unlock()
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if len(refs) > 0 {
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":      "proxy still referenced",
				"proxy":      name,
				"references": refs,
			})
			return
		}
		if err := db.DeleteProxy(name); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("proxies")
		s.mu.Unlock()
		util.LogModuleInfo("admin", "proxy deleted: %s", name)
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

func (s *AdminServer) apiToggleProxy(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/proxies/")
	if name == "" {
		httpError(w, "proxy name required", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, "decode fail", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	// Block disabling while the proxy is still referenced.
	if !body.Enabled {
		refs, err := s.findProxyReferences(name)
		if err != nil {
			s.mu.Unlock()
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if len(refs) > 0 {
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":      "proxy still referenced",
				"proxy":      name,
				"references": refs,
			})
			return
		}
	}
	defer s.mu.Unlock()
	p, err := db.GetProxy(name)
	if err != nil || p == nil {
		httpError(w, "proxy not found", http.StatusNotFound)
		return
	}
	p.Enabled = &body.Enabled
	if err := db.PutProxy(p); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after proxy toggle failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("proxies")
	jsonResponse(w, proxySummary(p))
}

func (s *AdminServer) apiToggleP2P(w http.ResponseWriter, r *http.Request) {
	util.LogModuleInfo("admin", "apiToggleP2P called: path=%s", r.URL.Path)
	name := strings.TrimPrefix(r.URL.Path, "/api/proxies/")
	name = strings.TrimSuffix(name, "/p2p")
	if name == "" {
		httpError(w, "proxy name required", http.StatusBadRequest)
		return
	}
	var body struct {
		P2P bool `json:"p2p"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, "decode fail", http.StatusBadRequest)
		return
	}
	util.LogModuleInfo("admin", "apiToggleP2P: name=%s, p2p=%v", name, body.P2P)

	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := db.GetProxy(name)
	if err != nil || p == nil {
		httpError(w, "proxy not found", http.StatusNotFound)
		return
	}
	if strings.EqualFold(p.Type, "h_tunnel") && !body.P2P {
		httpError(w, "h_tunnel requires P2P enabled (mesh downlink depends on it)", http.StatusBadRequest)
		return
	}
	p.P2P = &body.P2P
	if err := db.PutProxy(p); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after p2p toggle failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("proxies")
	util.LogModuleInfo("admin", "P2P %s for proxy %s", map[bool]string{true: "enabled", false: "disabled"}[body.P2P], name)
	jsonResponse(w, proxySummary(p))
}

func (s *AdminServer) apiRules(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		s.apiToggleRule(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		// Direct database read - skip config validation
		rules, err := db.ListRules()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		result := make([]map[string]interface{}, len(rules))
		for i, rule := range rules {
			trimmed := strings.TrimSpace(rule)
			result[i] = map[string]interface{}{
				"index":   i,
				"rule":    rule,
				"enabled": !strings.HasPrefix(trimmed, "//"),
			}
		}
		jsonResponse(w, result)

	case http.MethodPost:
		// Insert a rule at a specific position (or append if index not specified)
		var body struct {
			Rule  string `json:"rule"`
			Index *int   `json:"index"` // nil = append
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		if body.Rule == "" {
			httpError(w, "rule required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		// Direct database operation - skip config validation
		rules, err := db.ListRules()
		if err != nil {
			s.mu.Unlock()
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		idx := len(rules)
		if body.Index != nil {
			idx = *body.Index
			if idx < 0 {
				idx = 0
			}
			if idx > len(rules) {
				idx = len(rules)
			}
		}
		// Insert rule (single transaction handles re-indexing)
		if err := db.InsertRule(idx, body.Rule); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("rules")
		s.mu.Unlock()
		util.LogModuleInfo("admin", "rule inserted at %d: %s", idx, body.Rule)
		jsonResponse(w, map[string]interface{}{"status": "inserted", "index": idx})

	case http.MethodDelete:
		// Delete by index: /api/rules/3
		pathIdx := strings.TrimPrefix(r.URL.Path, "/api/rules/")
		idx, err := strconv.Atoi(pathIdx)
		if err != nil || idx < 0 {
			httpError(w, "invalid rule index", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		// Direct database operation - skip config validation to avoid deadlock
		rules, err := db.ListRules()
		if err != nil {
			s.mu.Unlock()
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if idx >= len(rules) {
			s.mu.Unlock()
			httpError(w, "invalid rule index", http.StatusBadRequest)
			return
		}
		deleted := rules[idx]
		// Delete rule (single transaction handles re-indexing)
		if err := db.DeleteRuleByIndex(idx); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("rules")
		s.mu.Unlock()
		util.LogModuleInfo("admin", "rule deleted at %d: %s", idx, deleted)
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

func (s *AdminServer) apiToggleRule(w http.ResponseWriter, r *http.Request) {
	pathIdx := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	idx, err := strconv.Atoi(pathIdx)
	if err != nil || idx < 0 {
		httpError(w, "invalid rule index", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, "decode fail", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Direct database operation - skip config validation
	rules, err := db.ListRules()
	if err != nil {
		httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if idx >= len(rules) {
		httpError(w, "invalid rule index", http.StatusBadRequest)
		return
	}
	rule := strings.TrimSpace(rules[idx])
	hasComment := strings.HasPrefix(rule, "//")
	if body.Enabled && hasComment {
		rules[idx] = strings.TrimPrefix(rule, "//")
	} else if !body.Enabled && !hasComment {
		rules[idx] = "//" + rule
	}
	if err := db.PutRule(idx, rules[idx]); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after rule toggle failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("rules")
	jsonResponse(w, map[string]interface{}{
		"index":   idx,
		"enabled": body.Enabled,
		"rule":    rules[idx],
	})
}

func (s *AdminServer) apiMappings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		s.apiToggleMapping(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		mappings, err := db.ListMappings()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		summaries := make([]map[string]interface{}, 0, len(mappings))
		for _, m := range mappings {
			summaries = append(summaries, mappingSummary(m))
		}
		jsonResponse(w, summaries)

	case http.MethodPost:
		var m config.Mapping
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		if m.Name == "" || m.Type == "" {
			httpError(w, "name and type required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		// Check if replacing an existing mapping
		existing, _ := db.GetMapping(m.Name)
		replaced := existing != nil
		var oldMapping *config.Mapping
		if replaced {
			oldCopy := *existing
			oldMapping = &oldCopy
		}

		if err := db.PutMapping(&m); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("mappings")
		s.mu.Unlock()
		// Notify runtime about mapping change
		if s.OnMappingUpdate != nil {
			if err := s.OnMappingUpdate(oldMapping, &m); err != nil {
				util.LogModuleWarn("admin", "mapping update callback failed: %v", err)
			}
		}
		if replaced {
			util.LogModuleInfo("admin", "mapping updated: %s (%s:%d)", m.Name, m.Type, m.Port)
			jsonResponse(w, mappingSummary(&m))
		} else {
			util.LogModuleInfo("admin", "mapping added: %s (%s:%d)", m.Name, m.Type, m.Port)
			w.WriteHeader(http.StatusCreated)
			jsonResponse(w, mappingSummary(&m))
		}

	case http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/api/mappings/")
		if name == "" {
			httpError(w, "mapping name required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		existing, err := db.GetMapping(name)
		if err != nil || existing == nil {
			s.mu.Unlock()
			httpError(w, "mapping not found", http.StatusNotFound)
			return
		}
		if err := db.DeleteMapping(name); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("mappings")
		s.mu.Unlock()
		// Notify runtime about mapping deletion
		if s.OnMappingUpdate != nil {
			if err := s.OnMappingUpdate(existing, nil); err != nil {
				util.LogModuleWarn("admin", "mapping delete callback failed: %v", err)
			}
		}
		util.LogModuleInfo("admin", "mapping deleted: %s", name)
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

func (s *AdminServer) apiToggleMapping(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/mappings/")
	if name == "" {
		httpError(w, "mapping name required", http.StatusBadRequest)
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, "decode fail", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := db.GetMapping(name)
	if err != nil || m == nil {
		httpError(w, "mapping not found", http.StatusNotFound)
		return
	}
	m.Enabled = &body.Enabled
	if err := db.PutMapping(m); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after mapping toggle failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("mappings")
	jsonResponse(w, mappingSummary(m))
}

func (s *AdminServer) apiResolvers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		name := strings.TrimPrefix(r.URL.Path, "/api/resolvers/")
		if name == "" {
			httpError(w, "resolver name required", http.StatusBadRequest)
			return
		}
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		rv, err := db.GetResolver(name)
		if err != nil || rv == nil {
			httpError(w, "resolver not found", http.StatusNotFound)
			return
		}
		rv.Enabled = &body.Enabled
		if err := db.PutResolver(rv); err != nil {
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogModuleWarn("admin", "incremental update after resolver toggle failed: %v", err)
			}
		}
		jsonResponse(w, resolverSummary(rv))
		return
	}
	switch r.Method {
	case http.MethodGet:
		resolvers, err := db.ListResolvers()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		summaries := make([]map[string]interface{}, 0, len(resolvers))
		for _, rv := range resolvers {
			summaries = append(summaries, resolverSummary(rv))
		}
		jsonResponse(w, summaries)

	case http.MethodPost:
		var rv config.Resolver
		if err := json.NewDecoder(r.Body).Decode(&rv); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		if rv.Name == "" {
			httpError(w, "name required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		existing, _ := db.GetResolver(rv.Name)
		replaced := existing != nil

		if err := db.PutResolver(&rv); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Unlock()
		if replaced {
			util.LogModuleInfo("admin", "resolver updated: %s (%s:%d -> %s:%d)", rv.Name, rv.SrcHost, rv.SrcPort, rv.DstHost, rv.DstPort)
			jsonResponse(w, resolverSummary(&rv))
		} else {
			util.LogModuleInfo("admin", "resolver added: %s (%s:%d -> %s:%d)", rv.Name, rv.SrcHost, rv.SrcPort, rv.DstHost, rv.DstPort)
			w.WriteHeader(http.StatusCreated)
			jsonResponse(w, resolverSummary(&rv))
		}

	case http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/api/resolvers/")
		if name == "" {
			httpError(w, "resolver name required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		if err := db.DeleteResolver(name); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Unlock()
		util.LogModuleInfo("admin", "resolver deleted: %s", name)
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

func resolverSummary(rv *config.Resolver) map[string]interface{} {
	if rv == nil {
		return nil
	}
	return map[string]interface{}{
		"name":        rv.Name,
		"enabled":     rv.IsEnabled(),
		"proto":       rv.Proto,
		"srcHost":     rv.SrcHost,
		"srcPort":     rv.SrcPort,
		"dstHost":     rv.DstHost,
		"dstPort":     rv.DstPort,
		"srcRelative": rv.SrcRelative,
		"srcFull":     rv.EffectiveSrcHost(),
	}
}

// apiGroupActions dispatches /api/groups/{name}/... sub-paths.
func (s *AdminServer) apiGroupActions(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		s.apiGroups(w, r)
		return
	}
	action := parts[1]
	switch action {
	case "subscription":
		s.apiGroupSubscription(w, r)
	case "health-check":
		s.apiGroupHealthCheck(w, r)
	case "members":
		s.apiGroupMembers(w, r)
	case "test":
		s.apiGroupTest(w, r)
	case "active-member":
		s.apiGroupActiveMember(w, r)
	case "toggle":
		s.apiToggleGroup(w, r)
	default:
		s.apiGroups(w, r)
	}
}

func (s *AdminServer) apiToggleGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	if len(parts) < 1 || parts[0] == "" {
		httpError(w, "group name required", http.StatusBadRequest)
		return
	}
	name := parts[0]
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, "decode fail", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	// Block disabling while the group is still referenced.
	if !body.Enabled {
		refs, err := s.findGroupReferences(name)
		if err != nil {
			s.mu.Unlock()
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if len(refs) > 0 {
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":      "group still referenced",
				"group":      name,
				"references": refs,
			})
			return
		}
	}
	defer s.mu.Unlock()
	g, err := db.GetProxyGroup(name)
	if err != nil || g == nil {
		httpError(w, "group not found", http.StatusNotFound)
		return
	}
	g.Enabled = &body.Enabled
	if err := db.PutProxyGroup(g); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after group toggle failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("proxies")
	jsonResponse(w, map[string]interface{}{
		"name":    g.Name,
		"enabled": g.IsEnabled(),
		"type":    g.Type,
		"proxies": g.Proxies,
	})
}

// apiGroupSubscription returns/updates a group's subscription selection and filter.
func (s *AdminServer) apiGroupSubscription(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "subscription" {
		httpError(w, "invalid subscription path", http.StatusBadRequest)
		return
	}
	groupName := parts[0]

	// Get group from database
	group, err := db.GetProxyGroup(groupName)
	if err != nil {
		httpError(w, "group not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		var nodes []map[string]interface{}
		var subName string
		if group.Subscription != "" {
			// Get subscription config from DB
			sub, err := db.GetSubscription(group.Subscription)
			if err == nil {
				subName = sub.Name
				// Get SubProxies from database
				proxies := s.getSubscriptionProxies(subName)
				names := make([]string, 0, len(proxies))
				for name := range proxies {
					names = append(names, name)
				}
				sort.Strings(names)
				nodes = make([]map[string]interface{}, 0, len(proxies))
				for _, name := range names {
					p := proxies[name]
					nodes = append(nodes, map[string]interface{}{
						"name":   name,
						"type":   p.Type,
						"server": p.Server,
						"port":   p.Port,
					})
				}
			}
		}
		// Get health from database
		healthList, _ := db.ListProxyHealthByGroup(groupName)
		healthMap := make(map[string]*db.ProxyHealth)
		for _, h := range healthList {
			healthMap[h.ProxyName] = h
		}
		respNodes := make([]map[string]interface{}, 0, len(nodes))
		for _, n := range nodes {
			name := n["name"].(string)
			key := "sub:" + name
			var alive bool
			var latencyMs int64
			var lastCheck time.Time
			if h, ok := healthMap[key]; ok {
				alive = h.Alive
				latencyMs = int64(h.Latency)
				lastCheck = h.LastCheck
			}
			respNodes = append(respNodes, map[string]interface{}{
				"name":      name,
				"type":      n["type"],
				"server":    n["server"],
				"port":      n["port"],
				"alive":     alive,
				"latencyMs": latencyMs,
				"lastCheck": lastCheck,
			})
		}
		jsonResponse(w, map[string]interface{}{
			"nodes":               respNodes,
			"active-member":       group.GetActiveMember(),
			"subscription":        subName,
			"type":                group.Type,
			"filter":              group.SubscriptionFilter,
			"healthCheckURL":      group.HealthCheckURL,
			"healthCheckInterval": group.HealthCheckInterval,
		})

	case http.MethodPut:
		var body struct {
			Filter       string `json:"filter"`
			ActiveMember string `json:"active-member"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		// Update the group in database directly
		group.SubscriptionFilter = body.Filter
		group.ActiveMember = body.ActiveMember
		group.RebuildProxies()
		if err := db.PutProxyGroup(group); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogModuleWarn("admin", "incremental update after subscription filter update failed: %v", err)
			}
		}
		util.DefaultVersionNotifier.BumpVersion("subscriptions")
		s.mu.Unlock()
		jsonResponse(w, map[string]interface{}{
			"filter":        body.Filter,
			"active-member": body.ActiveMember,
		})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiGroupHealthCheck triggers a one-off health check for a group or a single node.
func (s *AdminServer) apiGroupHealthCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "health-check" {
		httpError(w, "invalid health-check path", http.StatusBadRequest)
		return
	}
	groupName := parts[0]
	nodeName := ""
	if len(parts) >= 3 {
		nodeName = parts[2]
	}

	if nodeName != "" {
		if s.CheckGroupProxyHealth == nil {
			httpError(w, "proxy health check not configured", http.StatusServiceUnavailable)
			return
		}
		info, err := s.CheckGroupProxyHealth(groupName, nodeName)
		if err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]interface{}{
			"name":      nodeName,
			"alive":     info.Alive,
			"latencyMs": info.Latency.Milliseconds(),
			"lastCheck": info.LastCheck,
		})
		return
	}

	if s.CheckGroupHealth == nil {
		httpError(w, "health check not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.CheckGroupHealth(groupName); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return health snapshot from database.
	healthList, err := db.ListProxyHealthByGroup(groupName)
	if err != nil {
		httpError(w, "query health fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Convert to map format for JSON response
	snap := make(map[string]config.HealthInfo)
	for _, h := range healthList {
		snap[h.ProxyName] = config.HealthInfo{
			Alive:     h.Alive,
			Latency:   time.Duration(h.Latency) * time.Millisecond,
			LastCheck: h.LastCheck,
			FailCount: h.FailCount,
		}
	}
	jsonResponse(w, snap)
}

// apiGroupMembers returns the flattened member list for a group, including
// health state and the currently active member.
func (s *AdminServer) apiGroupMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "members" {
		httpError(w, "invalid members path", http.StatusBadRequest)
		return
	}
	groupName := parts[0]

	g, err := db.GetProxyGroup(groupName)
	if err != nil {
		httpError(w, "group not found", http.StatusNotFound)
		return
	}

	jsonResponse(w, buildGroupMemberList(g))
}

// apiGroupTest runs an immediate one-off health test for every member of a
// group and returns the resulting health snapshot.
func (s *AdminServer) apiGroupTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "test" {
		httpError(w, "invalid test path", http.StatusBadRequest)
		return
	}
	groupName := parts[0]

	if s.CheckGroupTest == nil {
		httpError(w, "group test not configured", http.StatusServiceUnavailable)
		return
	}
	if err := s.CheckGroupTest(groupName); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return the latest health snapshot from the database.
	healthList, err := db.ListProxyHealthByGroup(groupName)
	if err != nil {
		httpError(w, "query health fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := make(map[string]interface{}, len(healthList))
	for _, h := range healthList {
		resp[h.ProxyName] = map[string]interface{}{
			"alive":     h.Alive,
			"latencyMs": h.Latency,
			"lastCheck": h.LastCheck,
		}
	}
	jsonResponse(w, resp)
}

// apiGroupActiveMember sets the active member for a select group.
// The active member is an independent pointer into the current member list; it
// does not change group membership.
func (s *AdminServer) apiGroupActiveMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] != "active-member" {
		httpError(w, "invalid active-member path", http.StatusBadRequest)
		return
	}
	groupName := parts[0]

	var body struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, "decode fail", http.StatusBadRequest)
		return
	}
	if body.Name == "" {
		httpError(w, "name required", http.StatusBadRequest)
		return
	}
	if body.Source != "" && body.Source != "manual" && body.Source != "subscription" {
		httpError(w, "source must be manual or subscription", http.StatusBadRequest)
		return
	}

	// Get group from database
	runtimeGroup, err := db.GetProxyGroup(groupName)
	if err != nil {
		httpError(w, "group not found", http.StatusNotFound)
		return
	}
	if runtimeGroup.Type != config.GroupSelect {
		httpError(w, "only select groups support active member", http.StatusBadRequest)
		return
	}

	// Validate that the requested member exists in the current membership.
	wantSub := body.Source == "subscription"
	found := false
	for _, m := range runtimeGroup.GetMembers() {
		if m.Name != body.Name {
			continue
		}
		if body.Source != "" && m.FromSubscription != wantSub {
			continue
		}
		found = true
		break
	}
	if !found {
		httpError(w, "member not found in group", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	// Update the group in database directly
	if err := runtimeGroup.SetActiveMember(body.Name); err != nil {
		util.LogModuleWarn("admin", "SetActiveMember failed: %v", err)
	}
	if err := db.PutProxyGroup(runtimeGroup); err != nil {
		s.mu.Unlock()
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after active-member failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("subscriptions")
	s.mu.Unlock()
	jsonResponse(w, map[string]interface{}{
		"name":          body.Name,
		"source":        body.Source,
		"active-member": body.Name,
	})
}

// buildGroupMemberList builds the flattened member response for a group.
func buildGroupMemberList(g *config.ProxyGroup) []map[string]interface{} {
	members := g.GetMembers()
	// Get health from database instead of runtime snapshot
	healthList, _ := db.ListProxyHealthByGroup(g.Name)
	healthMap := make(map[string]*db.ProxyHealth)
	for _, h := range healthList {
		healthMap[h.ProxyName] = h
	}
	active := g.PickActiveMember()

	resp := make([]map[string]interface{}, 0, len(members))
	for _, m := range members {
		source := "manual"
		if m.FromSubscription {
			source = "subscription"
		}

		key := m.HealthKey()
		var alive bool
		var latencyMs int64
		var lastCheck time.Time
		if h, ok := healthMap[key]; ok {
			alive = h.Alive
			latencyMs = int64(h.Latency)
			lastCheck = h.LastCheck
		} else if !m.FromSubscription {
			// Manual proxies are considered alive until explicitly tested.
			alive = true
		}

		item := map[string]interface{}{
			"name":      m.Name,
			"source":    source,
			"isGroup":   m.IsGroup,
			"alive":     alive,
			"latencyMs": latencyMs,
			"lastCheck": lastCheck,
			"active":    m == active,
			"selected":  true,
		}

		if m.IsGroup {
			item["type"] = "group"
		} else {
			p := g.ResolveMember(m)
			if p != nil {
				item["type"] = p.Type
				item["server"] = p.Server
				item["port"] = p.Port
			}
		}
		resp = append(resp, item)
	}
	return resp
}

func (s *AdminServer) apiGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		groups, err := db.ListProxyGroups()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		summaries := make([]map[string]interface{}, 0, len(groups))
		for _, g := range groups {
			summaries = append(summaries, map[string]interface{}{
				"name":                   g.Name,
				"enabled":                g.IsEnabled(),
				"type":                   g.Type,
				"proxies":                g.Proxies,
				"manualProxies":          g.ManualProxies,
				"health-check-url":       g.HealthCheckURL,
				"health-check-interval":  g.HealthCheckInterval,
				"health-check-tolerance": g.HealthCheckTolerance,
				"lb-strategy":            g.LBStrategy,
				"subscription":           g.Subscription,
				"active-member":          g.ActiveMember,
				"subscription-filter":    g.SubscriptionFilter,
			})
		}
		jsonResponse(w, summaries)

	case http.MethodPost:
		var g config.ProxyGroup
		if err := json.NewDecoder(r.Body).Decode(&g); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		if g.Name == "" || g.Type == "" {
			httpError(w, "name and type required", http.StatusBadRequest)
			return
		}
		validTypes := map[string]bool{"select": true, "load-balance": true, "best": true}
		if !validTypes[g.Type] {
			httpError(w, "type must be select/load-balance/best", http.StatusBadRequest)
			return
		}
		g.SubscriptionMode = strings.ToLower(strings.TrimSpace(g.SubscriptionMode))
		g.SubscriptionMode = ""
		if len(g.Proxies) == 0 && g.Subscription == "" {
			httpError(w, "group must have at least one manual proxy or a subscription source", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		existing, _ := db.GetProxyGroup(g.Name)
		replaced := existing != nil

		g.ManualProxies = make([]string, len(g.Proxies))
		copy(g.ManualProxies, g.Proxies)

		if existing != nil {
			if g.Subscription != existing.Subscription {
				g.ActiveMember = ""
			} else {
				if g.ActiveMember == "" {
					g.ActiveMember = existing.ActiveMember
				}
			}
			if g.HealthCheckURL == "" {
				g.HealthCheckURL = existing.HealthCheckURL
			}
			if g.HealthCheckInterval == nil {
				g.HealthCheckInterval = existing.HealthCheckInterval
			}
			if g.HealthCheckTolerance == nil {
				g.HealthCheckTolerance = existing.HealthCheckTolerance
			}
			if g.LBStrategy == "" {
				g.LBStrategy = existing.LBStrategy
			}
			if g.SubscriptionFilter == "" {
				g.SubscriptionFilter = existing.SubscriptionFilter
			}
			g.CopyHealthFrom(existing)
		}

		if err := db.PutProxyGroup(&g); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogModuleWarn("admin", "incremental update after group add failed: %v", err)
			}
		}
		util.DefaultVersionNotifier.BumpVersion("proxies")
		s.mu.Unlock()
		if replaced {
			util.LogModuleInfo("admin", "group updated: %s (%s)", g.Name, g.Type)
		} else {
			util.LogModuleInfo("admin", "group added: %s (%s)", g.Name, g.Type)
			w.WriteHeader(http.StatusCreated)
		}
		jsonResponse(w, map[string]interface{}{
			"name":    g.Name,
			"type":    g.Type,
			"proxies": g.Proxies,
		})

	case http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/api/groups/")
		if name == "" {
			httpError(w, "group name required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		refs, err := s.findGroupReferences(name)
		if err != nil {
			s.mu.Unlock()
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if len(refs) > 0 {
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":      "group still referenced",
				"group":      name,
				"references": refs,
			})
			return
		}
		if err := db.DeleteProxyGroup(name); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("proxies")
		s.mu.Unlock()
		util.LogModuleInfo("admin", "group deleted: %s", name)
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

// apiSubscriptions handles CRUD for top-level subscriptions.
func (s *AdminServer) apiSubscriptions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		subs, err := db.ListSubscriptions()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		summaries := make([]map[string]interface{}, 0, len(subs))
		for _, sub := range subs {
			interval := 3600
			if sub.Interval != nil {
				interval = *sub.Interval
			}
			proxies := s.getSubscriptionProxies(sub.Name)
			nodeCount := len(proxies)
			summaries = append(summaries, map[string]interface{}{
				"name":      sub.Name,
				"url":       sub.URL,
				"interval":  interval,
				"enabled":   sub.IsEnabled(),
				"nodeCount": nodeCount,
			})
		}
		jsonResponse(w, summaries)

	case http.MethodPost:
		var req struct {
			Name     string `json:"name"`
			URL      string `json:"url"`
			Interval *int   `json:"interval"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		if req.Name == "" || req.URL == "" {
			httpError(w, "name and url required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		existing, _ := db.GetSubscription(req.Name)

		sub := config.Subscription{
			Name:     req.Name,
			URL:      req.URL,
			Interval: req.Interval,
		}
		if sub.Interval == nil {
			if existing != nil {
				sub.Interval = existing.Interval
			} else {
				v := 3600
				sub.Interval = &v
			}
		}

		// Preserve the node pool from database if the URL did not change
		sub.SubProxies = make(map[string]*config.Proxy)
		if existing != nil && existing.URL == sub.URL {
			existingProxies, _ := db.GetSubscriptionNodes(sub.Name)
			if existingProxies != nil {
				sub.SubProxies = existingProxies
			}
		}

		replaced := existing != nil
		if err := db.PutSubscription(&sub); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Persist node pool to database
		if len(sub.SubProxies) > 0 {
			if err := db.PutSubscriptionNodes(sub.Name, sub.SubProxies); err != nil {
				util.LogModuleWarn("admin", "persist subscription nodes failed: %v", err)
			}
		}
		util.DefaultVersionNotifier.BumpVersion("subscriptions")
		s.mu.Unlock()

		interval := 0
		if sub.Interval != nil {
			interval = *sub.Interval
		}
		if replaced {
			util.LogModuleInfo("admin", "subscription updated: %s", sub.Name)
		} else {
			util.LogModuleInfo("admin", "subscription added: %s", sub.Name)
			w.WriteHeader(http.StatusCreated)
		}
		jsonResponse(w, map[string]interface{}{
			"name":      sub.Name,
			"url":       sub.URL,
			"interval":  interval,
			"nodeCount": len(sub.SubProxies),
		})

	case http.MethodDelete:
		name := strings.TrimPrefix(r.URL.Path, "/api/subscriptions/")
		if name == "" {
			httpError(w, "subscription name required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		// Check if any group references this subscription
		groups, _ := db.ListProxyGroups()
		for _, g := range groups {
			if g.Subscription == name {
				s.mu.Unlock()
				httpError(w, "subscription is referenced by group "+g.Name, http.StatusBadRequest)
				return
			}
		}

		if err := db.DeleteSubscription(name); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("subscriptions")
		s.mu.Unlock()
		util.LogModuleInfo("admin", "subscription deleted: %s", name)
		jsonResponse(w, map[string]string{"status": "deleted"})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiSubscriptionActions dispatches /api/subscriptions/{name}/... sub-paths.
func (s *AdminServer) apiSubscriptionActions(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/subscriptions/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		s.apiSubscriptions(w, r)
		return
	}
	name := parts[0]
	action := parts[1]

	switch action {
	case "refresh":
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s.RefreshSubscription == nil {
			httpError(w, "subscription refresh not configured", http.StatusServiceUnavailable)
			return
		}
		if err := s.RefreshSubscription(name); err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		util.DefaultVersionNotifier.BumpVersion("subscriptions")

		nodeCount := 0
		proxies := s.getSubscriptionProxies(name)
		if proxies != nil {
			nodeCount = len(proxies)
		}
		jsonResponse(w, map[string]interface{}{
			"status":    "ok",
			"nodeCount": nodeCount,
		})

	case "toggle":
		if r.Method != http.MethodPatch {
			httpError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.apiToggleSubscription(w, r)

	case "nodes":
		if r.Method != http.MethodGet {
			httpError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Check subscription exists
		_, err := db.GetSubscription(name)
		if err != nil {
			httpError(w, "subscription not found", http.StatusNotFound)
			return
		}
		// Get subscription nodes from database
		proxies := s.getSubscriptionProxies(name)
		if proxies == nil {
			proxies = make(map[string]*config.Proxy)
		}
		names := make([]string, 0, len(proxies))
		for n := range proxies {
			names = append(names, n)
		}
		sort.Strings(names)

		// Collect the latest health info from any runtime group that references
		// this subscription so the unified node viewer can show initial status.
		healthByNode := make(map[string]config.HealthInfo)
		groups, _ := db.ListProxyGroups()
		for _, g := range groups {
			if g.Subscription != name {
				continue
			}
			snap := g.HealthSnapshot()
			for _, nodeName := range names {
				key := "sub:" + nodeName
				h, ok := snap[key]
				if !ok {
					continue
				}
				if existing, ok2 := healthByNode[nodeName]; !ok2 || h.LastCheck.After(existing.LastCheck) {
					healthByNode[nodeName] = h
				}
			}
		}

		nodes := make([]map[string]interface{}, len(names))
		for i, n := range names {
			p := proxies[n]
			h := healthByNode[n]
			nodes[i] = map[string]interface{}{
				"name":      n,
				"type":      p.Type,
				"server":    p.Server,
				"port":      p.Port,
				"alive":     h.Alive,
				"latencyMs": h.Latency.Milliseconds(),
				"lastCheck": h.LastCheck,
			}
		}
		jsonResponse(w, nodes)

	case "health-check":
		if r.Method != http.MethodPost {
			httpError(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if len(parts) < 3 {
			httpError(w, "node name required", http.StatusBadRequest)
			return
		}
		if s.CheckSubscriptionHealth == nil {
			httpError(w, "subscription health check not configured", http.StatusServiceUnavailable)
			return
		}
		nodeName := parts[2]
		checkURL := r.URL.Query().Get("url")
		info, err := s.CheckSubscriptionHealth(name, nodeName, checkURL)
		if err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]interface{}{
			"alive":     info.Alive,
			"latencyMs": info.Latency.Milliseconds(),
			"lastCheck": info.LastCheck,
		})

	default:
		httpError(w, "invalid subscription action", http.StatusBadRequest)
	}
}

func (s *AdminServer) apiToggleSubscription(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/subscriptions/")
	parts := strings.Split(path, "/")
	if len(parts) < 1 || parts[0] == "" {
		httpError(w, "subscription name required", http.StatusBadRequest)
		return
	}
	name := parts[0]
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, "decode fail", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	sub, err := db.GetSubscription(name)
	if err != nil || sub == nil {
		httpError(w, "subscription not found", http.StatusNotFound)
		return
	}
	sub.Enabled = &body.Enabled
	if err := db.PutSubscription(sub); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after subscription toggle failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("subscriptions")
	interval := 0
	if sub.Interval != nil {
		interval = *sub.Interval
	}
	nodeCount := 0
	proxies := s.getSubscriptionProxies(sub.Name)
	if proxies != nil {
		nodeCount = len(proxies)
	}
	jsonResponse(w, map[string]interface{}{
		"name":      sub.Name,
		"enabled":   sub.IsEnabled(),
		"url":       sub.URL,
		"interval":  interval,
		"nodeCount": nodeCount,
	})
}

func (s *AdminServer) apiHealth(w http.ResponseWriter, r *http.Request) {
	s.stats.CollectFromConfig()
	snapshot := s.stats.GetSnapshot()
	jsonResponse(w, snapshot["healthData"])
}

// reverseConfigHelpers (used by apiReverse and apiReverseItem)

func (s *AdminServer) getSubscriptionProxies(subName string) map[string]*config.Proxy {
	proxies, err := db.GetSubscriptionNodes(subName)
	if err != nil {
		return nil
	}
	return proxies
}

// currentReverseConfigs returns the active reverse config list from the database.
func (s *AdminServer) currentReverseConfigs() []*config.ReverseConfig {
	configs, err := db.ListReverseConfigs()
	if err != nil {
		return []*config.ReverseConfig{}
	}
	return configs
}

func cleanReverseName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == '#' || r == '?' {
			return '-'
		}
		return r
	}, n)
	return n
}

func uniqueReverseName(base string, configs []*config.ReverseConfig) string {
	names := make(map[string]bool)
	for _, c := range configs {
		if c != nil {
			names[c.Name] = true
		}
	}
	candidate := base
	if !names[candidate] {
		return candidate
	}
	for i := 1; i < 1000; i++ {
		candidate = fmt.Sprintf("%s-%d", base, i)
		if !names[candidate] {
			return candidate
		}
	}
	return base + "-x"
}

func hasReverseName(configs []*config.ReverseConfig, name string) bool {
	for _, c := range configs {
		if c != nil && c.Name == name {
			return true
		}
	}
	return false
}

// validateReverseConfig normalizes and validates a reverse config from the UI.
func (s *AdminServer) validateReverseConfig(rc *config.ReverseConfig) error {
	rc.Name = cleanReverseName(rc.Name)
	if rc.Name == "" {
		return fmt.Errorf("config name is required")
	}
	if rc.RegistryProxy == "" {
		return fmt.Errorf("registry proxy is required")
	}
	if rc.ListenerProto == "" {
		rc.ListenerProto = "socks5"
	}
	if rc.ReconnectInterval <= 0 {
		rc.ReconnectInterval = 10
	}
	if rc.ListenerProto == "direct" {
		if rc.TargetAddress == "" {
			return fmt.Errorf("direct listener requires target-address")
		}
		host, portStr, err := net.SplitHostPort(rc.TargetAddress)
		if err != nil {
			return fmt.Errorf("invalid target-address: %w", err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 {
			return fmt.Errorf("invalid target-address port")
		}
		rc.DirectDstHost = host
		rc.DirectDstPort = port
	} else {
		rc.DirectDstHost = ""
		rc.DirectDstPort = 0
	}
	return nil
}

func (s *AdminServer) apiReverse(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jsonResponse(w, s.reverseEventPayload(s.currentReverseConfigs()))

	case http.MethodPost:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpError(w, "read body fail", http.StatusBadRequest)
			return
		}

		var rc config.ReverseConfig
		if err := json.Unmarshal(body, &rc); err != nil {
			httpError(w, "parse config fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.validateReverseConfig(&rc); err != nil {
			httpError(w, err.Error(), http.StatusBadRequest)
			return
		}

		configs := s.currentReverseConfigs()
		rc.Name = uniqueReverseName(rc.Name, configs)

		s.mu.Lock()
		// Assign a seq number if not provided.
		if rc.Seq <= 0 {
			used := make(map[int]bool)
			for _, existing := range configs {
				if existing.Seq > 0 {
					used[existing.Seq] = true
				}
			}
			next := 1
			for used[next] {
				next++
			}
			rc.Seq = next
		}

		// Ensure instance ReverseID exists in file
		dataDir := filepath.Join("data", "setup")
		_, _ = reverse.GetReverseID(dataDir)

		if err := db.PutReverseConfig(&rc); err != nil {
			s.mu.Unlock()
			httpError(w, "save config fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Unlock()

		if s.OnReverseConfigUpdate != nil {
			if err := s.OnReverseConfigUpdate(nil, &rc); err != nil {
				util.LogModuleWarn("admin", "reverse config start after create failed: %v", err)
			}
		}

		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogModuleWarn("admin", "incremental update after reverse config create failed: %v", err)
			}
		}
		util.DefaultVersionNotifier.BumpVersion("reverse")
		util.LogModuleInfo("admin", "reverse config created: %s", rc.Name)
		jsonResponse(w, &rc)

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *AdminServer) apiReverseItem(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/reverse/")
	if strings.HasSuffix(name, "/toggle") {
		name = cleanReverseName(strings.TrimSuffix(name, "/toggle"))
		if name == "" {
			httpError(w, "name required", http.StatusBadRequest)
			return
		}
		s.apiReverseToggle(w, r, name)
		return
	}
	name = cleanReverseName(name)
	if name == "" {
		httpError(w, "name required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpError(w, "read body fail", http.StatusBadRequest)
			return
		}

		var rc config.ReverseConfig
		if err := json.Unmarshal(body, &rc); err != nil {
			httpError(w, "parse config fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.validateReverseConfig(&rc); err != nil {
			httpError(w, err.Error(), http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		oldRc, err := db.GetReverseConfig(name)
		if err != nil || oldRc == nil {
			s.mu.Unlock()
			httpError(w, "reverse config not found", http.StatusNotFound)
			return
		}
		oldCopy := *oldRc

		if err := db.PutReverseConfig(&rc); err != nil {
			s.mu.Unlock()
			httpError(w, "save config fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Unlock()

		if s.OnReverseConfigUpdate != nil {
			if err := s.OnReverseConfigUpdate(&oldCopy, &rc); err != nil {
				util.LogModuleWarn("admin", "reverse config update callback failed: %v", err)
			}
		}

		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogModuleWarn("admin", "incremental update after reverse config update failed: %v", err)
			}
		}
		util.DefaultVersionNotifier.BumpVersion("reverse")
		util.LogModuleInfo("admin", "reverse config updated: %s", rc.Name)
		jsonResponse(w, true)

	case http.MethodDelete:
		s.mu.Lock()
		removedConfig, err := db.GetReverseConfig(name)
		if err != nil || removedConfig == nil {
			s.mu.Unlock()
			httpError(w, "reverse config not found", http.StatusNotFound)
			return
		}

		if err := db.DeleteReverseConfig(name); err != nil {
			s.mu.Unlock()
			httpError(w, "save config fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Unlock()

		if s.OnReverseConfigUpdate != nil {
			if err := s.OnReverseConfigUpdate(removedConfig, nil); err != nil {
				util.LogModuleWarn("admin", "reverse config delete callback failed: %v", err)
			}
		}

		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogModuleWarn("admin", "incremental update after reverse config delete failed: %v", err)
			}
		}
		util.DefaultVersionNotifier.BumpVersion("reverse")
		util.LogModuleInfo("admin", "reverse config deleted: %s", name)
		jsonResponse(w, map[string]string{"status": "ok"})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *AdminServer) apiReverseToggle(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPatch {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "parse fail", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	existing, err := db.GetReverseConfig(name)
	if err != nil || existing == nil {
		s.mu.Unlock()
		httpError(w, "reverse config not found", http.StatusNotFound)
		return
	}
	oldCopy := *existing
	existing.Enabled = req.Enabled

	if err := db.PutReverseConfig(existing); err != nil {
		s.mu.Unlock()
		httpError(w, "save config fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Unlock()

	if s.OnReverseConfigUpdate != nil {
		if err := s.OnReverseConfigUpdate(&oldCopy, existing); err != nil {
			util.LogModuleWarn("admin", "reverse config toggle callback failed: %v", err)
		}
	}

	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogModuleWarn("admin", "incremental update after reverse config toggle failed: %v", err)
		}
	}
	util.DefaultVersionNotifier.BumpVersion("reverse")
	util.LogModuleInfo("admin", "reverse config %s toggled: enabled=%v", name, req.Enabled)
	jsonResponse(w, true)
}

func (s *AdminServer) apiReverseBindings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if s.GetReverseBindings == nil {
			jsonResponse(w, []interface{}{})
			return
		}
		bindings := s.GetReverseBindings()
		if bindings == nil {
			bindings = []server.PortBinding{}
		}
		jsonResponse(w, bindings)
	case http.MethodDelete:
		path := strings.TrimPrefix(r.URL.Path, "/api/reverse/bindings/")
		parts := strings.Split(path, "/")
		if len(parts) != 2 {
			httpError(w, "invalid path", http.StatusBadRequest)
			return
		}
		reverseID := parts[0]
		if reverseID == "" {
			httpError(w, "reverseID required", http.StatusBadRequest)
			return
		}
		seq, err := strconv.Atoi(parts[1])
		if err != nil || seq < 0 {
			httpError(w, "invalid seq", http.StatusBadRequest)
			return
		}
		if s.ForceRemoveBinding == nil {
			httpError(w, "not available", http.StatusServiceUnavailable)
			return
		}
		if err := s.ForceRemoveBinding(reverseID, seq); err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, map[string]string{"status": "ok"})
	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *AdminServer) apiP2P(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if p2p.GlobalP2PManager == nil {
		jsonResponse(w, map[string]interface{}{"peers": []interface{}{}})
		return
	}
	jsonResponse(w, map[string]interface{}{"peers": p2p.GlobalP2PManager.GetPeers()})
}

func (s *AdminServer) apiMesh(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/mesh")
	switch {
	case path == "" || path == "/":
		s.apiMeshGet(w, r)
	case path == "/config":
		s.apiMeshConfigPatch(w, r)
	case path == "/gossip":
		s.apiMeshGossipPost(w, r)
	case path == "/topology":
		s.apiMeshTopologyGet(w, r)
	default:
		httpError(w, "not found", http.StatusNotFound)
	}
}

func (s *AdminServer) apiMeshGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if mesh.GlobalMeshManager == nil {
		jsonResponse(w, map[string]interface{}{"enabled": false})
		return
	}
	result := mesh.GlobalMeshManager.GetStatus()
	result["topology"] = mesh.GlobalMeshManager.GetTopology()
	result["routes"] = mesh.GlobalMeshManager.GetRoutes()
	result["peers"] = mesh.GlobalMeshManager.GetPeers()
	result["linkNICStats"] = mesh.GlobalMeshManager.GetLinkNICStats()
	result["bestPaths"] = mesh.GlobalMeshManager.GetBestPaths()
	peerChannelDrops, meshInboundDrops := mesh.GlobalMeshManager.GetChannelDropStats()
	result["channelDropStats"] = map[string]interface{}{
		"peers":            peerChannelDrops,
		"meshInboundDrops": meshInboundDrops,
	}
	if cidr := mesh.GetMeshCIDR(); cidr != nil {
		result["meshCIDR"] = cidr.String()
	}
	jsonResponse(w, result)
}

func (s *AdminServer) apiMeshConfigPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if mesh.GlobalMeshManager == nil {
		httpError(w, "mesh not configured", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		NodeID         string   `json:"node-id"`
		DomainSuffixes []string `json:"domainSuffixes"`
		Advertise      []string `json:"advertise"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, cidr := range req.Advertise {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			httpError(w, "invalid CIDR: "+cidr, http.StatusBadRequest)
			return
		}
	}

	mesh.GlobalMeshManager.UpdateConfig(req.DomainSuffixes, req.Advertise)

	s.mu.Lock()
	meshCfg, _ := db.GetMeshConfig()
	if meshCfg != nil {
		if req.NodeID != "" {
			meshCfg.NodeID = req.NodeID
		}
		meshCfg.DomainSuffixes = req.DomainSuffixes
		meshCfg.Advertise = req.Advertise
	}
	if err := db.PutMeshConfig(meshCfg); err != nil {
		s.mu.Unlock()
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Unlock()

	util.DefaultVersionNotifier.BumpVersion("mesh")
	util.LogModuleInfo("admin", "mesh config updated: domainSuffixes=%v advertise=%v", req.DomainSuffixes, req.Advertise)
	jsonResponse(w, map[string]interface{}{"ok": true})
}

func (s *AdminServer) apiMeshGossipPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if mesh.GlobalMeshManager == nil {
		httpError(w, "mesh not configured", http.StatusServiceUnavailable)
		return
	}
	mesh.GlobalMeshManager.TriggerGossip()
	jsonResponse(w, map[string]interface{}{"ok": true})
}

func (s *AdminServer) apiMeshTopologyGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if mesh.GlobalMeshManager == nil {
		jsonResponse(w, map[string]interface{}{"enabled": false, "nodes": []interface{}{}, "edges": []interface{}{}})
		return
	}
	result := mesh.GlobalMeshManager.GetFullTopology()
	result["enabled"] = true
	jsonResponse(w, result)
}

// apiMeshDomainSuffixes handles GET/POST for /api/mesh/domain-suffixes
func (s *AdminServer) apiMeshDomainSuffixes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		suffixes := []string{}
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg != nil && meshCfg.DomainSuffixes != nil {
			suffixes = meshCfg.DomainSuffixes
		}
		jsonResponse(w, suffixes)

	case http.MethodPost:
		var req struct {
			Suffix string `json:"suffix"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Suffix == "" {
			httpError(w, "suffix required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg != nil {
			meshCfg.DomainSuffixes = append(meshCfg.DomainSuffixes, req.Suffix)
			if err := db.PutMeshConfig(meshCfg); err != nil {
				s.mu.Unlock()
				httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if mesh.GlobalMeshManager != nil {
				mesh.GlobalMeshManager.UpdateConfig(meshCfg.DomainSuffixes, meshCfg.Advertise)
			}
		}
		s.mu.Unlock()

		util.DefaultVersionNotifier.BumpVersion("mesh")
		util.LogModuleInfo("admin", "mesh domain-suffix added: %s", req.Suffix)
		jsonResponse(w, map[string]interface{}{"ok": true})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiMeshDomainSuffixItem handles PUT/DELETE for /api/mesh/domain-suffixes/:index
func (s *AdminServer) apiMeshDomainSuffixItem(w http.ResponseWriter, r *http.Request) {
	indexStr := strings.TrimPrefix(r.URL.Path, "/api/mesh/domain-suffixes/")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		httpError(w, "invalid index", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			Suffix string `json:"suffix"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Suffix == "" {
			httpError(w, "suffix required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg == nil || index < 0 || index >= len(meshCfg.DomainSuffixes) {
			s.mu.Unlock()
			httpError(w, "index out of range", http.StatusBadRequest)
			return
		}
		meshCfg.DomainSuffixes[index] = req.Suffix
		if err := db.PutMeshConfig(meshCfg); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if mesh.GlobalMeshManager != nil {
			mesh.GlobalMeshManager.UpdateConfig(meshCfg.DomainSuffixes, meshCfg.Advertise)
		}
		s.mu.Unlock()

		util.DefaultVersionNotifier.BumpVersion("mesh")
		util.LogModuleInfo("admin", "mesh domain-suffix updated: index=%d suffix=%s", index, req.Suffix)
		jsonResponse(w, map[string]interface{}{"ok": true})

	case http.MethodDelete:
		s.mu.Lock()
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg == nil || index < 0 || index >= len(meshCfg.DomainSuffixes) {
			s.mu.Unlock()
			httpError(w, "index out of range", http.StatusBadRequest)
			return
		}
		meshCfg.DomainSuffixes = append(meshCfg.DomainSuffixes[:index], meshCfg.DomainSuffixes[index+1:]...)
		if err := db.PutMeshConfig(meshCfg); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if mesh.GlobalMeshManager != nil {
			mesh.GlobalMeshManager.UpdateConfig(meshCfg.DomainSuffixes, meshCfg.Advertise)
		}
		s.mu.Unlock()

		util.DefaultVersionNotifier.BumpVersion("mesh")
		util.LogModuleInfo("admin", "mesh domain-suffix deleted: index=%d", index)
		jsonResponse(w, map[string]interface{}{"ok": true})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiMeshAdvertise handles GET/POST for /api/mesh/advertise
func (s *AdminServer) apiMeshAdvertise(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		advertise := []string{}
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg != nil {
			advertise = meshCfg.Advertise
		}
		jsonResponse(w, advertise)

	case http.MethodPost:
		var req struct {
			CIDR string `json:"cidr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, _, err := net.ParseCIDR(req.CIDR); err != nil {
			httpError(w, "invalid CIDR: "+req.CIDR, http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg == nil {
			s.mu.Unlock()
			httpError(w, "mesh config not found", http.StatusNotFound)
			return
		}
		meshCfg.Advertise = append(meshCfg.Advertise, req.CIDR)
		if err := db.PutMeshConfig(meshCfg); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if mesh.GlobalMeshManager != nil {
			mesh.GlobalMeshManager.UpdateConfig(meshCfg.DomainSuffixes, meshCfg.Advertise)
		}
		s.mu.Unlock()

		util.DefaultVersionNotifier.BumpVersion("mesh")
		util.LogModuleInfo("admin", "mesh advertise added: %s", req.CIDR)
		jsonResponse(w, map[string]interface{}{"ok": true})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiMeshAdvertiseItem handles PUT/DELETE for /api/mesh/advertise/:index
func (s *AdminServer) apiMeshAdvertiseItem(w http.ResponseWriter, r *http.Request) {
	indexStr := strings.TrimPrefix(r.URL.Path, "/api/mesh/advertise/")
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		httpError(w, "invalid index", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			CIDR string `json:"cidr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, _, err := net.ParseCIDR(req.CIDR); err != nil {
			httpError(w, "invalid CIDR: "+req.CIDR, http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg == nil || index < 0 || index >= len(meshCfg.Advertise) {
			s.mu.Unlock()
			httpError(w, "index out of range", http.StatusBadRequest)
			return
		}
		meshCfg.Advertise[index] = req.CIDR
		if err := db.PutMeshConfig(meshCfg); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if mesh.GlobalMeshManager != nil {
			mesh.GlobalMeshManager.UpdateConfig(meshCfg.DomainSuffixes, meshCfg.Advertise)
		}
		s.mu.Unlock()

		util.DefaultVersionNotifier.BumpVersion("mesh")
		util.LogModuleInfo("admin", "mesh advertise updated: index=%d cidr=%s", index, req.CIDR)
		jsonResponse(w, map[string]interface{}{"ok": true})

	case http.MethodDelete:
		s.mu.Lock()
		meshCfg, _ := db.GetMeshConfig()
		if meshCfg == nil || index < 0 || index >= len(meshCfg.Advertise) {
			s.mu.Unlock()
			httpError(w, "index out of range", http.StatusBadRequest)
			return
		}
		meshCfg.Advertise = append(meshCfg.Advertise[:index], meshCfg.Advertise[index+1:]...)
		if err := db.PutMeshConfig(meshCfg); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if mesh.GlobalMeshManager != nil {
			mesh.GlobalMeshManager.UpdateConfig(meshCfg.DomainSuffixes, meshCfg.Advertise)
		}
		s.mu.Unlock()

		util.DefaultVersionNotifier.BumpVersion("mesh")
		util.LogModuleInfo("admin", "mesh advertise deleted: index=%d", index)
		jsonResponse(w, map[string]interface{}{"ok": true})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *AdminServer) apiTUN(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		status := map[string]interface{}{
			"available":     tun.Available(),
			"enabled":       true,
			"bypassGateway": false,
			"dhcpEnabled":   false,
			"running":       false,
			"deviceName":    "",
			"routes":        tun.RouteSnapshot{},
			"logs":          []string{},
		}
		if s.GetTUNStatus != nil {
			if runtime := s.GetTUNStatus(); runtime != nil {
				for k, v := range runtime {
					status[k] = v
				}
			}
		} else {
			// Fallback: derive from DB config
			tunCfg, _ := db.GetTUNConfig()
			if tunCfg != nil {
				if tunCfg.Enabled != nil {
					status["enabled"] = *tunCfg.Enabled
				}
				status["bypassGateway"] = tunCfg.IsBypassGateway()
				status["dhcpEnabled"] = tunCfg.IsDHCPEnabled()
				if tunCfg.DHCP != nil {
					status["dhcpStaticBindings"] = tunCfg.DHCP.StaticBindings
				}
			}
		}
		jsonResponse(w, status)

	case http.MethodPatch:
		var req struct {
			Enabled            *bool                      `json:"enabled"`
			BypassGateway      *bool                      `json:"bypassGateway"`
			DHCPEnabled        *bool                      `json:"dhcpEnabled"`
			DHCPStaticBindings []config.DHCPStaticBinding `json:"dhcpStaticBindings"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail", http.StatusBadRequest)
			return
		}

		// TUN enabled toggle — requires callback to start/stop the engine.
		if req.Enabled != nil {
			if s.OnTUNToggle == nil {
				httpError(w, "TUN toggle not available", http.StatusServiceUnavailable)
				return
			}
			if err := s.OnTUNToggle(*req.Enabled); err != nil {
				httpError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			s.mu.Lock()
			tunCfg, _ := db.GetTUNConfig()
			if tunCfg == nil {
				tunCfg = &config.TUNConfig{}
			}
			tunCfg.Enabled = req.Enabled
			if err := db.PutTUNConfig(tunCfg); err != nil {
				s.mu.Unlock()
				httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
				return
			}
			s.mu.Unlock()
			util.LogModuleInfo("admin", "TUN toggle enabled=%v", *req.Enabled)
			util.DefaultVersionNotifier.BumpVersion("tun")
		}

		// Bypass gateway toggle — config-only, takes effect on next TUN start.
		if req.BypassGateway != nil {
			s.mu.Lock()
			tunCfg, _ := db.GetTUNConfig()
			if tunCfg == nil {
				tunCfg = &config.TUNConfig{}
			}
			tunCfg.BypassGateway = req.BypassGateway
			if err := db.PutTUNConfig(tunCfg); err != nil {
				s.mu.Unlock()
				httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
				return
			}
			s.mu.Unlock()
			util.LogModuleInfo("admin", "TUN bypass-gateway=%v", *req.BypassGateway)
			util.DefaultVersionNotifier.BumpVersion("tun")
		}

		// DHCP toggle — config-only, takes effect on next TUN start.
		if req.DHCPEnabled != nil {
			s.mu.Lock()
			tunCfg, _ := db.GetTUNConfig()
			if tunCfg == nil {
				tunCfg = &config.TUNConfig{}
			}
			if tunCfg.DHCP == nil {
				tunCfg.DHCP = &config.DHCPConfig{}
			}
			tunCfg.DHCP.Enabled = req.DHCPEnabled
			if err := db.PutTUNConfig(tunCfg); err != nil {
				s.mu.Unlock()
				httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
				return
			}
			s.mu.Unlock()
			util.LogModuleInfo("admin", "TUN dhcp-enabled=%v", *req.DHCPEnabled)
			util.DefaultVersionNotifier.BumpVersion("tun")
		}

		// DHCP static bindings — update config and hot-reload running server.
		if req.DHCPStaticBindings != nil {
			// Auto-generate ID for bindings without one
			existingIDs := make(map[int]bool)
			for _, b := range req.DHCPStaticBindings {
				if b.ID != "" {
					if id, err := strconv.Atoi(b.ID); err == nil {
						existingIDs[id] = true
					}
				}
			}
			nextID := 1
			for i := range req.DHCPStaticBindings {
				if req.DHCPStaticBindings[i].ID == "" {
					for existingIDs[nextID] {
						nextID++
					}
					req.DHCPStaticBindings[i].ID = strconv.Itoa(nextID)
					existingIDs[nextID] = true
					nextID++
				}
			}
			s.mu.Lock()
			tunCfg, _ := db.GetTUNConfig()
			if tunCfg == nil {
				tunCfg = &config.TUNConfig{}
			}
			if tunCfg.DHCP == nil {
				tunCfg.DHCP = &config.DHCPConfig{}
			}
			tunCfg.DHCP.StaticBindings = req.DHCPStaticBindings
			if err := db.PutTUNConfig(tunCfg); err != nil {
				s.mu.Unlock()
				httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
				return
			}
			s.mu.Unlock()
			if s.OnDHCPStaticBindingsUpdate != nil {
				s.OnDHCPStaticBindingsUpdate(req.DHCPStaticBindings)
			}
			util.LogModuleInfo("admin", "DHCP static-bindings updated (%d entries)", len(req.DHCPStaticBindings))
			util.DefaultVersionNotifier.BumpVersion("tun")
		}

		jsonResponse(w, map[string]string{"status": "ok"})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *AdminServer) apiCaptcha(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cleanupCaptchas()
	ip := clientIP(r)
	code := randomDigits(captchaDigits)
	storeCaptcha(ip, code)
	png, err := drawCaptcha(code)
	if err != nil {
		httpError(w, "captcha generation failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write(png)
}

func (s *AdminServer) apiLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Captcha  string `json:"captcha"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "parse fail", http.StatusBadRequest)
		return
	}
	ip := clientIP(r)
	needCaptcha := captchaRequiredFor(ip)
	if needCaptcha {
		if req.Captcha == "" || !verifyCaptcha(ip, req.Captcha) {
			recordFailedLogin(ip)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error":           "invalid captcha",
				"captchaRequired": true,
			})
			return
		}
	}
	if req.Username != s.config.Username || req.Password != s.config.Password {
		recordFailedLogin(ip)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error":           "invalid credentials",
			"captchaRequired": captchaRequiredFor(ip),
		})
		return
	}
	clearFailedLogins(ip)
	expires := time.Now().Add(sessionDuration).Unix()
	session := s.signSession(req.Username, expires)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionDuration),
	})
	jsonResponse(w, map[string]string{"status": "ok"})
}

func (s *AdminServer) apiLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	jsonResponse(w, map[string]string{"status": "ok"})
}

func (s *AdminServer) apiMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// If auth is disabled, return a default username
	if !s.config.AuthEnabled {
		jsonResponse(w, map[string]string{"username": "admin"})
		return
	}
	username := s.verifySession(r)
	if username == "" {
		httpError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	jsonResponse(w, map[string]string{"username": username})
}

func (s *AdminServer) apiAdminAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AuthEnabled bool   `json:"auth-enabled"`
		Username    string `json:"username"`
		Password    string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "parse fail", http.StatusBadRequest)
		return
	}

	// If auth is currently enabled, require existing session or token
	if s.config.AuthEnabled {
		if s.verifySession(r) == "" {
			token := r.URL.Query().Get("token")
			if token == "" {
				token = r.Header.Get("X-Admin-Token")
			}
			if token != s.config.Token {
				httpError(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
	}

	s.mu.Lock()
	s.config.AuthEnabled = req.AuthEnabled
	if req.Username != "" {
		s.config.Username = req.Username
	}
	if req.Password != "" {
		s.config.Password = req.Password
	}
	s.mu.Unlock()

	// Save to database
	if err := db.PutAdminConfig(s.config); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	s.mu.Unlock()

	util.LogModuleInfo("admin", "config persisted to database")
	util.DefaultVersionNotifier.BumpVersion("config")

	// If enabling auth and credentials were just set, create session immediately
	if req.AuthEnabled && req.Username != "" && req.Password != "" {
		expires := time.Now().Add(sessionDuration).Unix()
		session := s.signSession(req.Username, expires)
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    session,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(sessionDuration),
		})
	}

	util.LogModuleInfo("admin", "auth settings updated")
	jsonResponse(w, map[string]string{"status": "ok"})
}

func (s *AdminServer) apiSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		AuthEnabled    bool                    `json:"auth-enabled"`
		Username       string                  `json:"username"`
		Password       string                  `json:"password"`
		ReverseConfigs []*config.ReverseConfig `json:"reverse-configs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "parse fail", http.StatusBadRequest)
		return
	}

	// Update auth settings
	s.mu.Lock()
	if req.Username != "" {
		s.config.AuthEnabled = req.AuthEnabled
		s.config.Username = req.Username
		s.config.Password = req.Password
	}
	s.mu.Unlock()

	// Save reverse configs to runtime config (setup wizard always replaces the list)
	if len(req.ReverseConfigs) > 0 {
		existing := s.currentReverseConfigs()
		newNames := make(map[string]bool)
		for _, rc := range req.ReverseConfigs {
			if rc == nil {
				continue
			}
			rc.Enabled = true
			// Names and ReverseIDs must be unique across existing configs and within the new list.
			base := rc.Name
			if base == "" {
				base = "reverse"
			}
			candidate := base
			for i := 1; i < 1000; i++ {
				if !newNames[candidate] && !hasReverseName(existing, candidate) {
					break
				}
				candidate = fmt.Sprintf("%s-%d", base, i)
			}
			rc.Name = candidate
			newNames[candidate] = true
		}

		// Ensure instance ReverseID exists
		dataDir := filepath.Join("data", "setup")
		_, _ = reverse.GetReverseID(dataDir)
	}

	// Save admin config to database
	if err := db.PutAdminConfig(s.config); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Save reverse configs to database
	if len(req.ReverseConfigs) > 0 {
		for _, rc := range req.ReverseConfigs {
			if rc == nil {
				continue
			}
			if err := db.PutReverseConfig(rc); err != nil {
				httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	s.mu.Lock()
	s.mu.Unlock()

	util.LogModuleInfo("admin", "config persisted to database")
	util.DefaultVersionNotifier.BumpVersion("config")

	// Create session if auth enabled
	if req.AuthEnabled && req.Username != "" {
		expires := time.Now().Add(sessionDuration).Unix()
		session := s.signSession(req.Username, expires)
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    session,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Expires:  time.Now().Add(sessionDuration),
		})
	}

	// Trigger reload
	if s.OnReload != nil {
		s.OnReload()
	}

	util.LogModuleInfo("admin", "setup wizard completed")
	jsonResponse(w, map[string]string{"status": "ok"})
}

// ========== Helpers ==========

func (s *AdminServer) render(w http.ResponseWriter, r *http.Request, pageName string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Ensure every page has a non-empty Version for static-file cache busting.
	// Use GetCurrentVersion (git describe format) for consistency across all pages.
	if m, ok := data.(map[string]interface{}); ok {
		if v, _ := m["Version"].(string); v == "" {
			if s.GetCurrentVersion != nil {
				if version := s.GetCurrentVersion(); version != "" {
					m["Version"] = version
				} else {
					m["Version"] = adminVersion
				}
			} else {
				m["Version"] = adminVersion
			}
		}
	}

	var t *template.Template
	switch pageName {
	case "dashboard.html":
		t = s.pages.dashboard
	case "tun.html":
		t = s.pages.tun
	case "mesh.html":
		t = s.pages.mesh
	case "logs.html":
		t = s.pages.logs
	case "connections.html":
		t = s.pages.connections
	case "proxies.html":
		t = s.pages.proxies
	case "subscriptions.html":
		t = s.pages.subscriptions
	case "rules.html":
		t = s.pages.rules
	case "mappings.html":
		t = s.pages.mappings
	case "resolvers.html":
		t = s.pages.resolvers
	case "reverse-wizard.html":
		t = s.pages.reverseWizard
	case "package.html":
		t = s.pages.packagePage
	case "login.html":
		t = s.pages.login
	case "setup.html":
		t = s.pages.setup
	default:
		util.LogModuleError("admin", "unknown page: %s", pageName)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		if err := t.ExecuteTemplate(w, "content", data); err != nil {
			util.LogModuleError("admin", "fragment template error (%s): %v", pageName, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	if err := t.Execute(w, data); err != nil {
		util.LogModuleError("admin", "template error (%s): %v", pageName, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func jsonResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

// proxySummary returns a safe-to-serialize summary of a Proxy (no circular Next refs).
func proxySummary(p *config.Proxy) map[string]interface{} {
	if p == nil {
		return nil
	}
	return map[string]interface{}{
		"name":                  p.Name,
		"enabled":               p.IsEnabled(),
		"type":                  p.Type,
		"server":                p.Server,
		"port":                  p.Port,
		"sni":                   p.Sni,
		"udp":                   p.IsUDP(),
		"p2p":                   p.IsP2P(),
		"v2":                    p.IsV2(),
		"via":                   p.ViaProxy,
		"skip-cert-verify":      p.SkipCertVerify,
		"password":              p.Password,
		"url":                   p.URL,
		"username":              p.Username,
		"flow":                  p.Flow,
		"reverse-address":       p.ReverseAddress,
		"private-key":           p.PrivateKey,
		"private-key-passphrase": p.PrivateKeyPassphrase,
		"health-check-url":      p.HealthCheckURL,
	}
}

// mappingSummary returns a safe-to-serialize summary of a Mapping.
func mappingSummary(m *config.Mapping) map[string]interface{} {
	if m == nil {
		return nil
	}
	return map[string]interface{}{
		"name":           m.Name,
		"enabled":        m.IsEnabled(),
		"type":           m.Type,
		"port":           m.Port,
		"reverseAddress": m.ReverseAddress,
		"dstHost":        m.DstHost,
		"dstPort":        m.DstPort,
		"sni":            m.Sni,
		"password":       m.Password,
	}
}

func httpError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// generateSelfSignedCert generates a self-signed TLS certificate and key.
// Returns PEM-encoded certificate and key.
func generateSelfSignedCert() (certPEM, keyPEM []byte, err error) {
	// Generate private key
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key fail: %w", err)
	}

	// Create certificate template
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"Phaethon"},
			CommonName:   "Phaethon Admin",
		},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost", "*.phn"},
	}

	// Create certificate
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, fmt.Errorf("create certificate fail: %w", err)
	}

	// Encode to PEM
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})

	return certPEM, keyPEM, nil
}

// loadOrGenerateTLSCert loads TLS cert/key from files or generates and saves them.
func (s *AdminServer) loadOrGenerateTLSCert() (certPEM, keyPEM []byte, err error) {
	certFile := "admin-tls-cert.pem"
	keyFile := "admin-tls-key.pem"

	// Try to load existing cert and key
	certData, certErr := os.ReadFile(certFile)
	keyData, keyErr := os.ReadFile(keyFile)
	if certErr == nil && keyErr == nil && len(certData) > 0 && len(keyData) > 0 {
		util.LogModuleInfo("admin", "loaded TLS cert from %s and %s", certFile, keyFile)
		return certData, keyData, nil
	}

	// Generate new cert and key
	certPEM, keyPEM, err = generateSelfSignedCert()
	if err != nil {
		return nil, nil, err
	}

	// Save to files
	if err := os.WriteFile(certFile, certPEM, 0644); err != nil {
		util.LogModuleWarn("admin", "failed to save TLS cert to %s: %v", certFile, err)
	} else {
		util.LogModuleInfo("admin", "saved TLS cert to %s", certFile)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0600); err != nil {
		util.LogModuleWarn("admin", "failed to save TLS key to %s: %v", keyFile, err)
	} else {
		util.LogModuleInfo("admin", "saved TLS key to %s", keyFile)
	}

	return certPEM, keyPEM, nil
}

// apiLogModules handles GET/PUT /api/log/modules.
// GET: returns all module log levels and the global level.
// PUT: sets module log levels. Body: {"modules": {"socks5": "debug", "trojan": "warn"}}
//
//	or {"global": "debug"} to set the global level.
func (s *AdminServer) apiLogModules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		modules := util.ListModuleLogLevels()
		result := make(map[string]string, len(modules))
		for k, v := range modules {
			result[k] = util.LogLevelString(v)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"global":  util.LogLevelString(util.GlobalLogLevel()),
			"modules": result,
		})

	case http.MethodPut:
		var req struct {
			Global  string            `json:"global"`
			Modules map[string]string `json:"modules"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if req.Global != "" {
			if level, ok := util.ParseLogLevel(req.Global); ok {
				util.SetLogLevel(level)
			} else {
				http.Error(w, `{"error":"invalid global level"}`, http.StatusBadRequest)
				return
			}
		}
		applied := make(map[string]string)
		for module, levelStr := range req.Modules {
			if level, ok := util.ParseLogLevel(levelStr); ok {
				util.SetModuleLogLevel(module, level)
				applied[module] = levelStr
			}
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"global":  util.LogLevelString(util.GlobalLogLevel()),
			"applied": applied,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
