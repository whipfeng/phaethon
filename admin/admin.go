package admin

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"time"

	"gopkg.in/yaml.v3"

	"phaethon/cmd/setup"
	"phaethon/config"
	"phaethon/connlog"
	"phaethon/reverse"
	"phaethon/server"
	"phaethon/tun"
	"phaethon/util"
)

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
	config     *config.AdminConfig
	conf       *config.RuleConfiguration // merged runtime config (for stats/health)
	baseConf   *config.RuleConfiguration // base config loaded from confPath
	envConf    *config.RuleConfiguration // env config loaded from envPath (nil if none)
	stats      *StatsCollector
	server     *http.Server
	ln         net.Listener
	mu         sync.RWMutex
	confPath   string // path to the rule.yaml file for saving
	envPath    string // path to rule-{env}.yaml overlay (empty if none)
	saveTarget string // "base" or "env" — where edits are persisted

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

	// CheckProxyHealth runs a one-off health check for a single proxy/node in a group.
	// Set by the main package.
	CheckProxyHealth func(groupName, proxyName string) (config.HealthInfo, error)

	// CheckSubscriptionHealth runs a one-off health check for a single node
	// inside a subscription. Set by the main package.
	CheckSubscriptionHealth func(subName, nodeName, url string) (config.HealthInfo, error)

	// CheckManualProxyHealth runs a one-off connectivity check for a top-level proxy.
	// Set by the main package.
	CheckManualProxyHealth func(proxyName string) (config.HealthInfo, error)

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

	// templates
	pages *pageTemplates

	// defaultRaw holds the embedded default config bytes, used for reset/fallback.
	defaultRaw []byte

	// sessionSecret is a per-process random fallback HMAC key for signed cookies,
	// used only when the admin token is empty. It is generated at startup so that
	// deployments without an explicit token are still protected against cookie forgery.
	sessionSecret []byte
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
func NewAdminServer(conf *config.RuleConfiguration, ac *config.AdminConfig, defaultRaw []byte) *AdminServer {
	if ac == nil {
		ac = &config.AdminConfig{Enabled: false}
	}
	if ac.Addr == "" {
		ac.Addr = ":39999"
	}

	s := &AdminServer{
		config:     ac,
		conf:       conf,
		stats:      NewStatsCollector(),
		confPath:   "./config.yaml",
		saveTarget: "base",
		defaultRaw: defaultRaw,
	}

	// Generate a per-process random fallback session secret so deployments
	// without an explicit admin.token are not protected by a hardcoded default.
	s.sessionSecret = make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, s.sessionSecret); err != nil {
		util.LogError("[ADMIN] failed to generate session secret: %v", err)
		s.sessionSecret = nil
	}

	// Load base config (fall back to embedded default if no config.yaml exists)
	if base, err := config.LoadRaw(s.confPath); err == nil {
		_ = base.Init()
		s.baseConf = base
	} else if len(defaultRaw) > 0 {
		if base, err := config.LoadRawBytes(defaultRaw); err == nil {
			_ = base.Init()
			s.baseConf = base
		} else {
			s.baseConf = &config.RuleConfiguration{}
		}
	} else {
		s.baseConf = &config.RuleConfiguration{}
	}

	s.parseTemplates()
	return s
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

// displayConf returns the config currently being edited (base or env).
func (s *AdminServer) displayConf() *config.RuleConfiguration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.saveTarget == "env" && s.envConf != nil {
		return s.envConf
	}
	return s.baseConf
}

// mergeAndInitLocked rebuilds the runtime merged config from base + env and calls Init().
// Caller MUST already hold s.mu (write lock).
func (s *AdminServer) mergeAndInitLocked() error {
	if s.conf == nil {
		return fmt.Errorf("config not initialized")
	}

	// Deep-copy the base config via YAML to get fresh data
	raw, err := yaml.Marshal(s.baseConf)
	if err != nil {
		return fmt.Errorf("marshal base config: %w", err)
	}
	fresh := &config.RuleConfiguration{}
	if err := yaml.Unmarshal(raw, fresh); err != nil {
		return fmt.Errorf("unmarshal base config: %w", err)
	}
	if s.envConf != nil {
		if err := fresh.Merge(s.envConf); err != nil {
			return fmt.Errorf("merge fail: %w", err)
		}
	} else {
		if err := fresh.Init(); err != nil {
			return fmt.Errorf("init fail: %w", err)
		}
	}

	// Preserve runtime subscription state across admin edits
	oldSubByName := make(map[string]*config.Subscription)
	for _, sub := range s.conf.Subscriptions {
		oldSubByName[sub.Name] = sub
	}
	for _, sub := range fresh.Subscriptions {
		old, ok := oldSubByName[sub.Name]
		if !ok {
			continue
		}
		old.SubMu.RLock()
		subCopy := make(map[string]*config.Proxy, len(old.SubProxies))
		for n, p := range old.SubProxies {
			subCopy[n] = p
		}
		old.SubMu.RUnlock()
		sub.SubMu.Lock()
		sub.SubProxies = subCopy
		sub.SubMu.Unlock()
	}

	// Preserve per-group active member and filter
	oldGroupByName := make(map[string]*config.ProxyGroup)
	for _, g := range s.conf.ProxyGroups {
		oldGroupByName[g.Name] = g
	}
	for _, g := range fresh.ProxyGroups {
		old, ok := oldGroupByName[g.Name]
		if !ok {
			continue
		}
		if g.Subscription == old.Subscription {
			if g.ActiveMember == "" {
				g.ActiveMember = old.ActiveMember
			}
			if g.SubscriptionFilter == "" {
				g.SubscriptionFilter = old.SubscriptionFilter
			}
		}
		g.RebuildProxies()
	}

	// Update s.conf fields in place so all references see the changes
	s.conf.Proxies = fresh.Proxies
	s.conf.ProxyGroups = fresh.ProxyGroups
	s.conf.Subscriptions = fresh.Subscriptions
	s.conf.Rules = fresh.Rules
	s.conf.Mappings = fresh.Mappings
	s.conf.ReverseConfigs = fresh.ReverseConfigs
	s.conf.Matchers = fresh.Matchers
	s.conf.ProxyNames = fresh.ProxyNames
	s.conf.GroupNames = fresh.GroupNames
	s.conf.SubscriptionNames = fresh.SubscriptionNames

	return nil
}

// pageTemplates holds parsed templates for each page (layout + page content).
// Each entry is isolated so {{define "content"}} blocks don't conflict.
type pageTemplates struct {
	dashboard     *template.Template
	proxies       *template.Template
	subscriptions *template.Template
	rules         *template.Template
	mappings      *template.Template
	reverseWizard *template.Template
	login         *template.Template
	setup         *template.Template
	config        *template.Template
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
			splitTarget := func(raw string) (string, string) {
				idx := strings.Index(raw, "#")
				if idx < 0 {
					return raw, ""
				}
				return raw[:idx], raw[idx+1:]
			}
			r := strings.TrimSpace(rule)
			r = strings.TrimPrefix(r, "//")
			parts := strings.Split(r, ",")
			if len(parts) == 1 {
				target, mapping := splitTarget(parts[0])
				return []string{parts[0], "", target, mapping}
			}
			if len(parts) == 2 {
				target, mapping := splitTarget(parts[1])
				return []string{parts[0], "", target, mapping}
			}
			keyword := parts[0]
			rawTarget := parts[len(parts)-1]
			value := strings.Join(parts[1:len(parts)-1], ",")
			target, mapping := splitTarget(rawTarget)
			return []string{keyword, value, target, mapping}
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
		proxies:       parsePage("proxies.html"),
		subscriptions: parsePage("subscriptions.html"),
		rules:         parsePage("rules.html"),
		mappings:      parsePage("mappings.html"),
		reverseWizard: parsePage("reverse-wizard.html"),
		login:         parseStandalone("login.html"),
		setup:         parseStandalone("setup.html"),
		config:        parsePage("config.html"),
	}
}

// Start launches the admin HTTP server.
func (s *AdminServer) Start() error {
	if !s.config.Enabled {
		util.LogInfo("[ADMIN] disabled, skipping")
		return nil
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	addr := s.config.Addr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin listen on %s fail: %w", addr, err)
	}
	s.ln = ln

	s.server = &http.Server{
		Handler:      s.securityHeadersMiddleware(s.authMiddleware(mux)),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Use TLS if certificate and key are configured
	if s.config.TLSCert != "" && s.config.TLSKey != "" {
		util.LogInfo("[ADMIN] starting on https://%s", addr)
		go func() {
			if err := s.server.ServeTLS(ln, s.config.TLSCert, s.config.TLSKey); err != nil && err != http.ErrServerClosed {
				util.LogError("[ADMIN] serve error: %v", err)
			}
		}()
	} else {
		util.LogInfo("[ADMIN] starting on http://%s", addr)
		go func() {
			if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
				util.LogError("[ADMIN] serve error: %v", err)
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
func isPublicPath(path string) bool {
	return path == "/login" || path == "/api/login" || path == "/api/captcha" || path == "/setup" || path == "/api/setup" ||
		strings.HasPrefix(path, "/static/")
}

// authMiddleware checks authentication (session or token).
func (s *AdminServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If auth is not enabled, allow everything
		if !s.config.AuthEnabled {
			next.ServeHTTP(w, r)
			return
		}

		// Public paths are always accessible
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Try session-based login first
		if user := s.verifySession(r); user != "" {
			next.ServeHTTP(w, r)
			return
		}

		// Fall back to token-based auth (for API/programmatic access)
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

		// Not authenticated — redirect to login for pages, 401 for API
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
	mux.HandleFunc("/proxies", s.handleProxiesPage)
	mux.HandleFunc("/subscriptions", s.handleSubscriptionsPage)
	mux.HandleFunc("/rules", s.handleRulesPage)
	mux.HandleFunc("/mappings", s.handleMappingsPage)
	mux.HandleFunc("/reverse", s.handleReverseWizardPage)
	mux.HandleFunc("/logs", s.handleLogsPage)
	mux.HandleFunc("/config", s.handleConfigPage)
	mux.HandleFunc("/login", s.handleLoginPage)
	mux.HandleFunc("/setup", s.handleSetupPage)

	// API endpoints — use HandleFunc with path checks for RESTful routing
	mux.HandleFunc("/api/stats", s.apiStats)
	mux.HandleFunc("/api/config", s.apiConfig)
	mux.HandleFunc("/api/config/raw", s.apiConfigRaw)
	mux.HandleFunc("/api/config/reset", s.apiConfigReset)
	mux.HandleFunc("/api/config/reload", s.apiReload)
	mux.HandleFunc("/api/config/target", s.apiTarget)
	mux.HandleFunc("/api/proxies", s.apiProxies)
	mux.HandleFunc("/api/proxies/health-check/", s.apiProxyHealthCheck)
	mux.HandleFunc("/api/proxies/", s.apiProxies)
	mux.HandleFunc("/api/rules", s.apiRules)
	mux.HandleFunc("/api/rules/", s.apiRules)
	mux.HandleFunc("/api/mappings", s.apiMappings)
	mux.HandleFunc("/api/mappings/", s.apiMappings)
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
	mux.HandleFunc("/api/tun", s.apiTUN)
	mux.HandleFunc("/api/events", s.apiEvents)
	mux.HandleFunc("/api/versions", s.apiVersions)
	mux.HandleFunc("/api/login", s.apiLogin)
	mux.HandleFunc("/api/captcha", s.apiCaptcha)
	mux.HandleFunc("/api/logout", s.apiLogout)
	mux.HandleFunc("/api/me", s.apiMe)
	mux.HandleFunc("/api/admin/auth", s.apiAdminAuth)
	mux.HandleFunc("/api/setup", s.apiSetup)
}

// ========== Page Handlers ==========

func (s *AdminServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Runtime stats from merged config
	s.mu.RLock()
	runtimeConf := s.conf
	saveTarget := s.saveTarget
	envPath := s.envPath
	s.mu.RUnlock()

	s.stats.CollectFromConfig(runtimeConf)

	// Counts from the config being edited
	dc := s.displayConf()

	data := map[string]interface{}{
		"Title":             "Dashboard",
		"Version":           os.Getenv("PHAETHON_VERSION"),
		"Stats":             s.stats.GetSnapshot(),
		"UptimeSeconds":     int64(time.Since(s.stats.StartTime()).Seconds()),
		"ProxyCount":        len(dc.Proxies),
		"RuleCount":         len(dc.Rules),
		"MappingCount":      len(dc.Mappings),
		"GroupCount":        len(dc.ProxyGroups),
		"SubscriptionCount": len(dc.Subscriptions),
		"EnvInfo":           s.envInfo(),
		"SaveTarget":        saveTarget,
		"CanSwitch":         envPath != "",
		"TUNAvailable":      tun.Available(),
	}
	s.render(w, r, "dashboard.html", data)
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
	if env != "" {
		dir := filepath.Dir(s.confPath)
		overlayPath := filepath.Join(dir, "rule-"+env+".yaml")
		if _, err := os.Stat(overlayPath); err == nil {
			info["HasOverlay"] = true
			info["OverlayPath"] = overlayPath
		}
	}
	return info
}

func (s *AdminServer) handleProxiesPage(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	s.mu.RLock()
	saveTarget := s.saveTarget
	envPath := s.envPath
	runtimeConf := s.conf
	s.mu.RUnlock()

	// Show runtime subscription selections on the page without mutating the
	// editable base/env config.  We copy the base groups and overlay the
	// runtime Proxies / ManualProxies so the card totals reflect reality.
	displayGroups := dc.ProxyGroups
	groupStats := make(map[string]map[string]interface{})
	if runtimeConf != nil {
		runtimeGroups := make(map[string]*config.ProxyGroup, len(runtimeConf.ProxyGroups))
		for _, g := range runtimeConf.ProxyGroups {
			runtimeGroups[g.Name] = g
		}
		displayGroups = make([]*config.ProxyGroup, 0, len(dc.ProxyGroups))
		for _, g := range dc.ProxyGroups {
			cp := *g
			if rg, ok := runtimeGroups[g.Name]; ok {
				cp.Proxies = rg.Proxies
				cp.ManualProxies = rg.ManualProxies
				cp.SubCandidateCount = len(rg.SubscriptionCandidates())
				active := rg.PickActiveMember()
				groupStats[g.Name] = map[string]interface{}{
					"total":  len(rg.GetMembers()),
					"active": active.Name,
				}
			} else {
				groupStats[g.Name] = map[string]interface{}{
					"total":  cp.SubCandidateCount + len(cp.ManualProxies),
					"active": "",
				}
			}
			displayGroups = append(displayGroups, &cp)
		}
	} else {
		for _, g := range dc.ProxyGroups {
			groupStats[g.Name] = map[string]interface{}{
				"total":  g.SubCandidateCount + len(g.ManualProxies),
				"active": "",
			}
		}
	}

	data := map[string]interface{}{
		"Title":         "Proxies",
		"Proxies":       dc.Proxies,
		"Groups":        displayGroups,
		"GroupStats":    groupStats,
		"Subscriptions": dc.Subscriptions,
		"SaveTarget":    saveTarget,
		"CanSwitch":     envPath != "",
	}
	s.render(w, r, "proxies.html", data)
}

func (s *AdminServer) handleSubscriptionsPage(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	s.mu.RLock()
	saveTarget := s.saveTarget
	envPath := s.envPath
	runtimeConf := s.conf
	s.mu.RUnlock()

	// Build subscription snapshot from runtime config so the page can show
	// live subscription nodes without leaking them into the global proxy list.
	subData := make(map[string]map[string]interface{})
	if runtimeConf != nil {
		for _, sub := range runtimeConf.Subscriptions {
			sub.SubMu.RLock()
			nodes := make([]map[string]interface{}, 0, len(sub.SubProxies))
			for name, p := range sub.SubProxies {
				nodes = append(nodes, map[string]interface{}{
					"name":   name,
					"type":   p.Type,
					"server": p.Server,
					"port":   p.Port,
				})
			}
			sub.SubMu.RUnlock()
			subData[sub.Name] = map[string]interface{}{
				"nodes": nodes,
				"url":   sub.URL,
			}
		}
	}

	data := map[string]interface{}{
		"Title":         "Subscriptions",
		"Subscriptions": dc.Subscriptions,
		"SubData":       subData,
		"SaveTarget":    saveTarget,
		"CanSwitch":     envPath != "",
	}
	s.render(w, r, "subscriptions.html", data)
}

func (s *AdminServer) handleRulesPage(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	s.mu.RLock()
	saveTarget := s.saveTarget
	envPath := s.envPath
	s.mu.RUnlock()
	data := map[string]interface{}{
		"Title":      "Rules",
		"Rules":      dc.Rules,
		"Proxies":    dc.Proxies,
		"Groups":     dc.ProxyGroups,
		"Mappings":   dc.Mappings,
		"SaveTarget": saveTarget,
		"CanSwitch":  envPath != "",
	}
	s.render(w, r, "rules.html", data)
}

func (s *AdminServer) handleMappingsPage(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	s.mu.RLock()
	saveTarget := s.saveTarget
	envPath := s.envPath
	s.mu.RUnlock()
	data := map[string]interface{}{
		"Title":      "Mappings",
		"Mappings":   dc.Mappings,
		"SaveTarget": saveTarget,
		"CanSwitch":  envPath != "",
	}
	s.render(w, r, "mappings.html", data)
}

func (s *AdminServer) handleReverseWizardPage(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	s.mu.RLock()
	saveTarget := s.saveTarget
	envPath := s.envPath
	s.mu.RUnlock()

	// Build proxy list with reverse-support flag
	type proxyInfo struct {
		*config.Proxy
		SupportsReverse bool
	}
	var proxies []proxyInfo
	for _, p := range dc.Proxies {
		supports := false
		switch p.Type {
		case "socks5", "trojan", "h_tunnel":
			supports = true
		}
		proxies = append(proxies, proxyInfo{p, supports})
	}

	data := map[string]interface{}{
		"Title":          "Reverse",
		"Proxies":        proxies,
		"ReverseConfigs": s.currentReverseConfigs(),
		"SaveTarget":     saveTarget,
		"CanSwitch":      envPath != "",
	}
	s.render(w, r, "reverse-wizard.html", data)
}

func (s *AdminServer) handleLogsPage(w http.ResponseWriter, r *http.Request) {
	data, err := templates.ReadFile("templates/logs-standalone.html")
	if err != nil {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func (s *AdminServer) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages.login.Execute(w, map[string]interface{}{
		"AuthEnabled": s.config.AuthEnabled,
	}); err != nil {
		util.LogError("[ADMIN] login template error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages.setup.Execute(w, map[string]interface{}{
		"AuthEnabled": s.config.AuthEnabled,
		"Username":    s.config.Username,
	}); err != nil {
		util.LogError("[ADMIN] setup template error: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func (s *AdminServer) handleConfigPage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	saveTarget := s.saveTarget
	envPath := s.envPath
	s.mu.RUnlock()

	data := map[string]interface{}{
		"Title":      "Raw Config",
		"SaveTarget": saveTarget,
		"CanSwitch":  envPath != "",
	}
	s.render(w, r, "config.html", data)
}

// ========== API Handlers ==========

func (s *AdminServer) apiStats(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()

	s.stats.CollectFromConfig(conf)
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
func reverseEventPayload(configs []*config.ReverseConfig) interface{} {
	items := make([]map[string]interface{}, 0, len(configs))
	for _, rc := range configs {
		if rc == nil {
			continue
		}
		items = append(items, map[string]interface{}{
			"name":          rc.Name,
			"reverseId":     rc.ReverseID,
			"seq":           rc.Seq,
			"enabled":       rc.Enabled,
			"registryAddr":  rc.RegistryAddr,
			"outboundProxy": rc.OutboundProxy,
			"registerProto": rc.RegisterProto,
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
			s.mu.RLock()
			conf := s.conf
			s.mu.RUnlock()

			s.stats.CollectFromConfig(conf)
		case <-s.sseStopCh:
			return
		}
	}
}

func (s *AdminServer) apiConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	switch r.Method {
	case http.MethodGet:
		// Return config without sensitive fields
		conf := sanitizeConfig(s.conf)
		jsonResponse(w, conf)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpError(w, "read body fail", http.StatusBadRequest)
			return
		}

		var newConf config.RuleConfiguration
		if err := json.Unmarshal(body, &newConf); err != nil {
			httpError(w, "parse config fail: "+err.Error(), http.StatusBadRequest)
			return
		}

		// Validate and init
		if err := newConf.Init(); err != nil {
			httpError(w, "init config fail: "+err.Error(), http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		s.conf = &newConf
		s.mu.Unlock()

		util.LogInfo("[ADMIN] config updated via API")
		jsonResponse(w, map[string]string{"status": "ok"})
	}
}

func (s *AdminServer) apiConfigRaw(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		target := s.currentConfigTargetLocked()
		s.mu.RUnlock()

		data, err := os.ReadFile(target)
		if err != nil {
			if len(s.defaultRaw) > 0 {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Write(s.defaultRaw)
				return
			}
			httpError(w, "read config fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(data)

	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			httpError(w, "read body fail", http.StatusBadRequest)
			return
		}
		if len(bytes.TrimSpace(body)) == 0 {
			httpError(w, "empty config", http.StatusBadRequest)
			return
		}

		// Validate before writing.
		newConf, err := config.LoadRawBytes(body)
		if err != nil {
			httpError(w, "parse config fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := newConf.Init(); err != nil {
			httpError(w, "init config fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		_ = newConf

		s.mu.Lock()
		target := s.currentConfigTargetLocked()
		if err := writeFileAtomic(target, body); err != nil {
			s.mu.Unlock()
			httpError(w, "write config fail: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Reload the edited layer from disk so in-memory state matches the file.
		if s.saveTarget == "env" && s.envPath != "" {
			if ec, err := config.LoadRaw(target); err == nil {
				_ = ec.Init()
				s.envConf = ec
			}
		} else {
			if base, err := config.LoadRaw(target); err == nil {
				_ = base.Init()
				s.baseConf = base
			}
		}

		if err := s.mergeAndInitLocked(); err != nil {
			s.mu.Unlock()
			util.LogWarn("[ADMIN] merge after raw config write failed: %v", err)
			httpError(w, "merge fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Unlock()

		util.LogInfo("[ADMIN] raw config written to %s (target=%s)", target, s.saveTarget)

		// Check if full reload was requested
		if r.URL.Query().Get("reload") == "true" {
			if s.OnReload != nil {
				go s.OnReload()
				jsonResponse(w, map[string]string{"status": "saved and reloading"})
				return
			}
		}
		jsonResponse(w, map[string]string{"status": "ok"})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiConfigReset restores the editable config to the embedded default.
// It deletes any env overlay and overwrites the base rule.yaml with defaultRaw,
// then triggers a full runtime reload.
func (s *AdminServer) apiConfigReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if len(s.defaultRaw) == 0 {
		httpError(w, "no default config available", http.StatusServiceUnavailable)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	target := s.currentConfigTargetLocked()
	if err := writeFileAtomic(target, s.defaultRaw); err != nil {
		httpError(w, "write default config fail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Remove env overlay if it exists.
	if s.envPath != "" {
		_ = os.Remove(s.envPath)
		s.envConf = nil
	}
	s.saveTarget = "base"

	if base, err := config.LoadRaw(target); err == nil {
		_ = base.Init()
		s.baseConf = base
	}

	if err := s.mergeAndInitLocked(); err != nil {
		util.LogWarn("[ADMIN] merge after reset failed: %v", err)
		httpError(w, "merge fail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if s.OnReload != nil {
		go s.OnReload()
	}

	util.LogInfo("[ADMIN] config reset to default: %s", target)
	jsonResponse(w, map[string]string{"status": "ok"})
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
	if s.CheckManualProxyHealth == nil {
		httpError(w, "manual proxy health check not configured", http.StatusServiceUnavailable)
		return
	}
	info, err := s.CheckManualProxyHealth(name)
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

	util.LogInfo("[ADMIN] reload triggered via API")
	jsonResponse(w, map[string]string{"status": "reload triggered"})
}

// currentConfigTargetLocked returns the on-disk path currently being edited.
// Caller must hold s.mu (read or write lock).
func (s *AdminServer) currentConfigTargetLocked() string {
	target := s.confPath
	if s.saveTarget == "env" && s.envPath != "" {
		target = s.envPath
	}
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, "rule.yaml")
	}
	return target
}

// writeFileAtomic writes data to path using a temporary file and rename.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *AdminServer) apiTarget(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		info := map[string]interface{}{
			"saveTarget": s.saveTarget,
			"basePath":   s.confPath,
			"envPath":    s.envPath,
			"canSwitch":  s.envPath != "",
		}
		s.mu.RUnlock()
		jsonResponse(w, info)

	case http.MethodPost:
		var body struct {
			Target string `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}
		if body.Target != "base" && body.Target != "env" {
			httpError(w, "target must be 'base' or 'env'", http.StatusBadRequest)
			return
		}
		if body.Target == "env" && s.envPath == "" {
			httpError(w, "no env overlay file detected", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		s.saveTarget = body.Target
		s.mu.Unlock()

		util.LogInfo("[ADMIN] save target switched to %s", body.Target)
		jsonResponse(w, map[string]interface{}{
			"status":     "ok",
			"saveTarget": body.Target,
		})
	}
}

// proxyReference describes one place where a proxy is still referenced.
type proxyReference struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// findProxyReferences returns every rule, group, proxy, or mapping that refers
// to the named proxy. It checks the merged runtime configuration.
func (s *AdminServer) findProxyReferences(name string, c *config.RuleConfiguration) []proxyReference {
	var refs []proxyReference
	if c == nil || name == "" {
		return refs
	}

	for _, rule := range c.Rules {
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

	for _, g := range c.ProxyGroups {
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

	for _, p := range c.Proxies {
		if p != nil && p.ProxyName == name {
			refs = append(refs, proxyReference{Kind: "proxy", Name: p.Name})
		}
	}

	for _, m := range c.Mappings {
		if m != nil && m.ReverseProxy == name {
			refs = append(refs, proxyReference{Kind: "mapping", Name: m.Name})
		}
	}

	for _, rc := range c.ReverseConfigs {
		if rc != nil && rc.OutboundProxy == name {
			refs = append(refs, proxyReference{Kind: "reverse", Name: rc.Name})
		}
	}

	return refs
}

// findGroupReferences returns every rule, group, proxy, mapping, or reverse
// config that refers to the named proxy group.
func (s *AdminServer) findGroupReferences(name string, c *config.RuleConfiguration) []proxyReference {
	var refs []proxyReference
	if c == nil || name == "" {
		return refs
	}

	for _, rule := range c.Rules {
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

	for _, g := range c.ProxyGroups {
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

	for _, p := range c.Proxies {
		if p != nil && p.ProxyName == name {
			refs = append(refs, proxyReference{Kind: "proxy", Name: p.Name})
		}
	}

	for _, m := range c.Mappings {
		if m != nil && m.ReverseProxy == name {
			refs = append(refs, proxyReference{Kind: "mapping", Name: m.Name})
		}
	}

	for _, rc := range c.ReverseConfigs {
		if rc != nil && rc.OutboundProxy == name {
			refs = append(refs, proxyReference{Kind: "reverse", Name: rc.Name})
		}
	}

	return refs
}

func (s *AdminServer) apiProxies(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	if r.Method == http.MethodPatch {
		s.apiToggleProxy(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		summaries := make([]map[string]interface{}, len(dc.Proxies))
		for i, p := range dc.Proxies {
			summaries[i] = proxySummary(p)
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
		replaced := false
		for i, existing := range dc.Proxies {
			if existing.Name == p.Name {
				dc.Proxies[i] = &p
				replaced = true
				break
			}
		}
		if !replaced {
			dc.Proxies = append(dc.Proxies, &p)
		}
		if err := dc.Init(); err != nil {
			if !replaced {
				dc.Proxies = dc.Proxies[:len(dc.Proxies)-1]
			}
			s.mu.Unlock()
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after proxy add failed: %v", err)
		}
		s.mu.Unlock()
		if replaced {
			util.LogInfo("[ADMIN] proxy updated in %s: %s (%s)", s.saveTarget, p.Name, p.Type)
			jsonResponse(w, proxySummary(&p))
		} else {
			util.LogInfo("[ADMIN] proxy added to %s: %s (%s)", s.saveTarget, p.Name, p.Type)
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
		if refs := s.findProxyReferences(name, s.conf); len(refs) > 0 {
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
		for i, p := range dc.Proxies {
			if p.Name == name {
				old := dc.Proxies
				dc.Proxies = append(dc.Proxies[:i], dc.Proxies[i+1:]...)
				if err := dc.Init(); err != nil {
					dc.Proxies = old
					s.mu.Unlock()
					httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
					return
				}
				if err := s.saveConfigLocked(); err != nil {
					s.mu.Unlock()
					httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
					return
				}
				if err := s.mergeAndInitLocked(); err != nil {
					util.LogWarn("[ADMIN] merge after proxy delete failed: %v", err)
				}
				s.mu.Unlock()
				util.LogInfo("[ADMIN] proxy deleted from %s: %s", s.saveTarget, name)
				jsonResponse(w, map[string]string{"status": "deleted"})
				return
			}
		}
		s.mu.Unlock()
		httpError(w, "proxy not found", http.StatusNotFound)
	}
}

func (s *AdminServer) apiToggleProxy(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
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
		if refs := s.findProxyReferences(name, s.conf); len(refs) > 0 {
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
	for _, p := range dc.Proxies {
		if p.Name != name {
			continue
		}
		p.Enabled = &body.Enabled
		if err := dc.Init(); err != nil {
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after proxy toggle failed: %v", err)
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after proxy toggle failed: %v", err)
			}
		}
		jsonResponse(w, proxySummary(p))
		return
	}
	httpError(w, "proxy not found", http.StatusNotFound)
}

func (s *AdminServer) apiRules(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	if r.Method == http.MethodPatch {
		s.apiToggleRule(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rules := make([]map[string]interface{}, len(dc.Rules))
		for i, rule := range dc.Rules {
			trimmed := strings.TrimSpace(rule)
			rules[i] = map[string]interface{}{
				"index":   i,
				"rule":    rule,
				"enabled": !strings.HasPrefix(trimmed, "//"),
			}
		}
		jsonResponse(w, rules)

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
		idx := len(dc.Rules)
		if body.Index != nil {
			idx = *body.Index
			if idx < 0 {
				idx = 0
			}
			if idx > len(dc.Rules) {
				idx = len(dc.Rules)
			}
		}
		// Insert at position
		dc.Rules = append(dc.Rules[:idx], append([]string{body.Rule}, dc.Rules[idx:]...)...)
		if err := dc.Init(); err != nil {
			// Rollback: remove inserted rule
			dc.Rules = append(dc.Rules[:idx], dc.Rules[idx+1:]...)
			s.mu.Unlock()
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after rule insert failed: %v", err)
		}
		s.mu.Unlock()
		util.LogInfo("[ADMIN] rule inserted at %d in %s: %s", idx, s.saveTarget, body.Rule)
		jsonResponse(w, map[string]interface{}{"status": "inserted", "index": idx})

	case http.MethodPut:
		// Full replace (bulk update)
		var rules []string
		if err := json.NewDecoder(r.Body).Decode(&rules); err != nil {
			httpError(w, "decode fail", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		oldRules := dc.Rules
		dc.Rules = rules
		if err := dc.Init(); err != nil {
			dc.Rules = oldRules
			s.mu.Unlock()
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after rules update failed: %v", err)
		}
		s.mu.Unlock()
		util.LogInfo("[ADMIN] rules updated in %s (%d rules)", s.saveTarget, len(rules))
		jsonResponse(w, map[string]interface{}{"status": "ok", "count": len(rules)})

	case http.MethodDelete:
		// Delete by index: /api/rules/3
		pathIdx := strings.TrimPrefix(r.URL.Path, "/api/rules/")
		idx, err := strconv.Atoi(pathIdx)
		if err != nil || idx < 0 || idx >= len(dc.Rules) {
			httpError(w, "invalid rule index", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		deleted := dc.Rules[idx]
		dc.Rules = append(dc.Rules[:idx], dc.Rules[idx+1:]...)
		if err := dc.Init(); err != nil {
			// Rollback is hard here; just re-insert
			dc.Rules = append(dc.Rules[:idx], append([]string{deleted}, dc.Rules[idx:]...)...)
			s.mu.Unlock()
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after rule delete failed: %v", err)
		}
		s.mu.Unlock()
		util.LogInfo("[ADMIN] rule deleted at %d from %s: %s", idx, s.saveTarget, deleted)
		jsonResponse(w, map[string]string{"status": "deleted"})
	}
}

func (s *AdminServer) apiToggleRule(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	pathIdx := strings.TrimPrefix(r.URL.Path, "/api/rules/")
	idx, err := strconv.Atoi(pathIdx)
	if err != nil || idx < 0 || idx >= len(dc.Rules) {
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
	rule := strings.TrimSpace(dc.Rules[idx])
	hasComment := strings.HasPrefix(rule, "//")
	if body.Enabled && hasComment {
		dc.Rules[idx] = strings.TrimPrefix(rule, "//")
	} else if !body.Enabled && !hasComment {
		dc.Rules[idx] = "//" + rule
	}
	if err := dc.Init(); err != nil {
		httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.saveConfigLocked(); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.mergeAndInitLocked(); err != nil {
		util.LogWarn("[ADMIN] merge after rule toggle failed: %v", err)
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogWarn("[ADMIN] incremental update after rule toggle failed: %v", err)
		}
	}
	jsonResponse(w, map[string]interface{}{
		"index":   idx,
		"enabled": body.Enabled,
		"rule":    dc.Rules[idx],
	})
}

func (s *AdminServer) apiMappings(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
	if r.Method == http.MethodPatch {
		s.apiToggleMapping(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		summaries := make([]map[string]interface{}, len(dc.Mappings))
		for i, m := range dc.Mappings {
			summaries[i] = mappingSummary(m)
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
		// Update in place if name already exists, otherwise append
		replaced := false
		var oldMapping *config.Mapping
		for i, existing := range dc.Mappings {
			if existing.Name == m.Name {
				oldMapping = existing
				dc.Mappings[i] = &m
				replaced = true
				break
			}
		}
		if !replaced {
			dc.Mappings = append(dc.Mappings, &m)
		}
		if err := dc.Init(); err != nil {
			if !replaced {
				dc.Mappings = dc.Mappings[:len(dc.Mappings)-1]
			}
			s.mu.Unlock()
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after mapping add failed: %v", err)
		}
		s.mu.Unlock()
		// Notify runtime about mapping change
		if s.OnMappingUpdate != nil {
			if err := s.OnMappingUpdate(oldMapping, &m); err != nil {
				util.LogWarn("[ADMIN] mapping update callback failed: %v", err)
			}
		}
		if replaced {
			util.LogInfo("[ADMIN] mapping updated in %s: %s (%s:%d)", s.saveTarget, m.Name, m.Type, m.Port)
			jsonResponse(w, mappingSummary(&m))
		} else {
			util.LogInfo("[ADMIN] mapping added to %s: %s (%s:%d)", s.saveTarget, m.Name, m.Type, m.Port)
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
		for i, m := range dc.Mappings {
			if m.Name == name {
				old := dc.Mappings
				dc.Mappings = append(dc.Mappings[:i], dc.Mappings[i+1:]...)
				if err := dc.Init(); err != nil {
					dc.Mappings = old
					s.mu.Unlock()
					httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
					return
				}
				if err := s.saveConfigLocked(); err != nil {
					s.mu.Unlock()
					httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
					return
				}
				if err := s.mergeAndInitLocked(); err != nil {
					util.LogWarn("[ADMIN] merge after mapping delete failed: %v", err)
				}
				s.mu.Unlock()
				// Notify runtime about mapping deletion
				if s.OnMappingUpdate != nil {
					if err := s.OnMappingUpdate(m, nil); err != nil {
						util.LogWarn("[ADMIN] mapping delete callback failed: %v", err)
					}
				}
				util.LogInfo("[ADMIN] mapping deleted from %s: %s", s.saveTarget, name)
				jsonResponse(w, map[string]string{"status": "deleted"})
				return
			}
		}
		s.mu.Unlock()
		httpError(w, "mapping not found", http.StatusNotFound)
	}
}

func (s *AdminServer) apiToggleMapping(w http.ResponseWriter, r *http.Request) {
	dc := s.displayConf()
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
	for _, m := range dc.Mappings {
		if m.Name != name {
			continue
		}
		m.Enabled = &body.Enabled
		if err := dc.Init(); err != nil {
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after mapping toggle failed: %v", err)
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after mapping toggle failed: %v", err)
			}
		}
		jsonResponse(w, mappingSummary(m))
		return
	}
	httpError(w, "mapping not found", http.StatusNotFound)
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
	dc := s.displayConf()
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
		if refs := s.findGroupReferences(name, s.conf); len(refs) > 0 {
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
	for _, g := range dc.ProxyGroups {
		if g.Name != name {
			continue
		}
		g.Enabled = &body.Enabled
		if err := dc.Init(); err != nil {
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after group toggle failed: %v", err)
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after group toggle failed: %v", err)
			}
		}
		jsonResponse(w, map[string]interface{}{
			"name":    g.Name,
			"enabled": g.IsEnabled(),
			"type":    g.Type,
			"proxies": g.Proxies,
		})
		return
	}
	httpError(w, "group not found", http.StatusNotFound)
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

	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()
	if conf == nil {
		httpError(w, "config not ready", http.StatusServiceUnavailable)
		return
	}

	var group *config.ProxyGroup
	for _, g := range conf.ProxyGroups {
		if g.Name == groupName {
			group = g
			break
		}
	}
	if group == nil {
		httpError(w, "group not found", http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		var nodes []map[string]interface{}
		var subName string
		if group.Subscription != "" {
			for _, sub := range conf.Subscriptions {
				if sub.Name == group.Subscription {
					subName = sub.Name
					sub.SubMu.RLock()
					names := make([]string, 0, len(sub.SubProxies))
					for name := range sub.SubProxies {
						names = append(names, name)
					}
					sort.Strings(names)
					nodes = make([]map[string]interface{}, 0, len(sub.SubProxies))
					for _, name := range names {
						p := sub.SubProxies[name]
						nodes = append(nodes, map[string]interface{}{
							"name":   name,
							"type":   p.Type,
							"server": p.Server,
							"port":   p.Port,
						})
					}
					sub.SubMu.RUnlock()
					break
				}
			}
		}
		health := group.HealthSnapshot()
		respNodes := make([]map[string]interface{}, 0, len(nodes))
		for _, n := range nodes {
			name := n["name"].(string)
			// Subscription nodes are keyed with the "sub:" prefix in the group's
			// health map so they do not collide with manual proxies of the same name.
			h, ok := health["sub:"+name]
			if !ok {
				h = config.HealthInfo{}
			}
			respNodes = append(respNodes, map[string]interface{}{
				"name":      name,
				"type":      n["type"],
				"server":    n["server"],
				"port":      n["port"],
				"alive":     h.Alive,
				"latencyMs": h.Latency.Milliseconds(),
				"lastCheck": h.LastCheck,
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
		// Persist filter/active changes into the config being edited (base or env).
		dc := s.displayConf()
		var dcGroup *config.ProxyGroup
		for _, gg := range dc.ProxyGroups {
			if gg.Name == groupName {
				dcGroup = gg
				break
			}
		}
		if dcGroup == nil {
			httpError(w, "group not found in editable config", http.StatusNotFound)
			return
		}
		dcGroup.SubscriptionFilter = body.Filter
		dcGroup.ActiveMember = body.ActiveMember

		s.mu.Lock()
		// Update the current runtime group while holding the write lock so the
		// merge step below preserves the new filter instead of a stale one.
		if s.conf != nil {
			for _, rg := range s.conf.ProxyGroups {
				if rg.Name == groupName {
					rg.SubscriptionFilter = body.Filter
					rg.ActiveMember = body.ActiveMember
					rg.RebuildProxies()
					break
				}
			}
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after subscription filter update failed: %v", err)
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after subscription filter update failed: %v", err)
			}
		}
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
		if s.CheckProxyHealth == nil {
			httpError(w, "proxy health check not configured", http.StatusServiceUnavailable)
			return
		}
		info, err := s.CheckProxyHealth(groupName, nodeName)
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

	// Return health snapshot for the group.
	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()
	for _, g := range conf.ProxyGroups {
		if g.Name == groupName {
			jsonResponse(w, g.HealthSnapshot())
			return
		}
	}
	httpError(w, "group not found", http.StatusNotFound)
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

	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()
	if conf == nil {
		httpError(w, "config not ready", http.StatusServiceUnavailable)
		return
	}

	for _, g := range conf.ProxyGroups {
		if g.Name == groupName {
			jsonResponse(w, buildGroupMemberList(g))
			return
		}
	}
	httpError(w, "group not found", http.StatusNotFound)
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

	// Return the latest health snapshot from the current runtime config.
	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()
	if conf == nil {
		httpError(w, "config not ready", http.StatusServiceUnavailable)
		return
	}
	for _, g := range conf.ProxyGroups {
		if g.Name != groupName {
			continue
		}
		snap := g.HealthSnapshot()
		resp := make(map[string]interface{}, len(snap))
		for key, hi := range snap {
			resp[key] = map[string]interface{}{
				"alive":     hi.Alive,
				"latencyMs": hi.Latency.Milliseconds(),
				"lastCheck": hi.LastCheck,
			}
		}
		jsonResponse(w, resp)
		return
	}
	httpError(w, "group not found", http.StatusNotFound)
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

	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()
	if conf == nil {
		httpError(w, "config not ready", http.StatusServiceUnavailable)
		return
	}

	var runtimeGroup *config.ProxyGroup
	for _, g := range conf.ProxyGroups {
		if g.Name == groupName {
			runtimeGroup = g
			break
		}
	}
	if runtimeGroup == nil {
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

	dc := s.displayConf()
	var dcGroup *config.ProxyGroup
	for _, gg := range dc.ProxyGroups {
		if gg.Name == groupName {
			dcGroup = gg
			break
		}
	}
	if dcGroup == nil {
		httpError(w, "group not found in editable config", http.StatusNotFound)
		return
	}
	dcGroup.ActiveMember = body.Name

	s.mu.Lock()
	if s.conf != nil {
		for _, rg := range s.conf.ProxyGroups {
			if rg.Name == groupName {
				if err := rg.SetActiveMember(body.Name); err != nil {
					util.LogWarn("[ADMIN] SetActiveMember failed: %v", err)
				}
				break
			}
		}
	}
	if err := s.saveConfigLocked(); err != nil {
		s.mu.Unlock()
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.mergeAndInitLocked(); err != nil {
		util.LogWarn("[ADMIN] merge after active-member failed: %v", err)
	}
	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogWarn("[ADMIN] incremental update after active-member failed: %v", err)
		}
	}
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
	health := g.HealthSnapshot()
	active := g.PickActiveMember()

	resp := make([]map[string]interface{}, 0, len(members))
	for _, m := range members {
		source := "manual"
		if m.FromSubscription {
			source = "subscription"
		}

		key := m.HealthKey()
		hi, ok := health[key]
		alive := false
		latencyMs := int64(0)
		var lastCheck time.Time
		if ok {
			alive = hi.Alive
			latencyMs = hi.Latency.Milliseconds()
			lastCheck = hi.LastCheck
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
	dc := s.displayConf()
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		runtimeConf := s.conf
		s.mu.RUnlock()
		runtimeGroups := make(map[string]*config.ProxyGroup)
		if runtimeConf != nil {
			for _, g := range runtimeConf.ProxyGroups {
				runtimeGroups[g.Name] = g
			}
		}
		summaries := make([]map[string]interface{}, len(dc.ProxyGroups))
		for i, g := range dc.ProxyGroups {
			proxies := g.Proxies
			manualProxies := g.ManualProxies
			activeMember := g.ActiveMember
			filter := g.SubscriptionFilter
			if rg, ok := runtimeGroups[g.Name]; ok {
				proxies = rg.Proxies
				manualProxies = rg.ManualProxies
				activeMember = rg.ActiveMember
				filter = rg.SubscriptionFilter
			}
			summaries[i] = map[string]interface{}{
				"name":                  g.Name,
				"enabled":               g.IsEnabled(),
				"type":                  g.Type,
				"proxies":               proxies,
				"manualProxies":         manualProxies,
				"health-check-url":      g.HealthCheckURL,
				"health-check-interval": g.HealthCheckInterval,
				"subscription":          g.Subscription,
				"active-member":         activeMember,
				"subscription-filter":   filter,
			}
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
		// subscription-mode is deprecated and ignored; clear it so it never
		// leaks back into the persisted YAML.
		g.SubscriptionMode = ""
		if len(g.Proxies) == 0 && g.Subscription == "" {
			httpError(w, "group must have at least one manual proxy or a subscription source", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		var existing *config.ProxyGroup
		for _, eg := range dc.ProxyGroups {
			if eg.Name == g.Name {
				existing = eg
				break
			}
		}

		// Persist manual nodes only in YAML. Runtime rebuild will add matching
		// subscription nodes on the next reload.
		g.ManualProxies = make([]string, len(g.Proxies))
		copy(g.ManualProxies, g.Proxies)
		// Keep g.Proxies as the YAML source of truth for manual members
		// since ManualProxies is yaml:"-".

		if existing != nil {
			// Accept the incoming subscription source. If the subscription
			// itself changed, the active member no longer applies.
			if g.Subscription != existing.Subscription {
				g.ActiveMember = ""
			} else {
				if g.ActiveMember == "" {
					g.ActiveMember = existing.ActiveMember
				}
			}
			// Preserve health-check settings and filter when the client did not
			// explicitly supply them.
			if g.HealthCheckURL == "" {
				g.HealthCheckURL = existing.HealthCheckURL
			}
			if g.HealthCheckInterval == nil {
				g.HealthCheckInterval = existing.HealthCheckInterval
			}
			if g.SubscriptionFilter == "" {
				g.SubscriptionFilter = existing.SubscriptionFilter
			}
			for i, eg := range dc.ProxyGroups {
				if eg.Name == g.Name {
					dc.ProxyGroups[i] = &g
					break
				}
			}
		} else {
			dc.ProxyGroups = append(dc.ProxyGroups, &g)
		}
		replaced := existing != nil

		if err := dc.Init(); err != nil {
			if !replaced {
				dc.ProxyGroups = dc.ProxyGroups[:len(dc.ProxyGroups)-1]
			}
			s.mu.Unlock()
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after group add failed: %v", err)
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after group add failed: %v", err)
			}
		}
		s.mu.Unlock()
		if replaced {
			util.LogInfo("[ADMIN] group updated in %s: %s (%s)", s.saveTarget, g.Name, g.Type)
		} else {
			util.LogInfo("[ADMIN] group added to %s: %s (%s)", s.saveTarget, g.Name, g.Type)
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
		// Block deletion while the group is still referenced.
		if refs := s.findGroupReferences(name, s.conf); len(refs) > 0 {
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
		for i, g := range dc.ProxyGroups {
			if g.Name == name {
				old := dc.ProxyGroups
				dc.ProxyGroups = append(dc.ProxyGroups[:i], dc.ProxyGroups[i+1:]...)
				if err := dc.Init(); err != nil {
					dc.ProxyGroups = old
					s.mu.Unlock()
					httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
					return
				}
				if err := s.saveConfigLocked(); err != nil {
					s.mu.Unlock()
					httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
					return
				}
				if err := s.mergeAndInitLocked(); err != nil {
					util.LogWarn("[ADMIN] merge after group delete failed: %v", err)
				}
				s.mu.Unlock()
				util.LogInfo("[ADMIN] group deleted from %s: %s", s.saveTarget, name)
				jsonResponse(w, map[string]string{"status": "deleted"})
				return
			}
		}
		s.mu.Unlock()
		httpError(w, "group not found", http.StatusNotFound)
	}
}

// apiSubscriptions handles CRUD for top-level subscriptions.
func (s *AdminServer) apiSubscriptions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		dc := s.displayConf()
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
		var existing *config.Subscription
		var existingIdx int
		for i, es := range dc.Subscriptions {
			if es.Name == req.Name {
				existing = es
				existingIdx = i
				break
			}
		}

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

		// Preserve the runtime node pool if the URL did not change; otherwise
		// start fresh so the next refresh populates the new pool.
		sub.SubProxies = make(map[string]*config.Proxy)
		if existing != nil && existing.URL == sub.URL {
			existing.SubMu.RLock()
			for n, p := range existing.SubProxies {
				sub.SubProxies[n] = p
			}
			existing.SubMu.RUnlock()
		}

		replaced := existing != nil
		if replaced {
			dc.Subscriptions[existingIdx] = &sub
		} else {
			dc.Subscriptions = append(dc.Subscriptions, &sub)
		}

		if err := dc.Init(); err != nil {
			if !replaced {
				dc.Subscriptions = dc.Subscriptions[:len(dc.Subscriptions)-1]
			}
			s.mu.Unlock()
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after subscription edit failed: %v", err)
		}
		s.mu.Unlock()

		interval := 0
		if sub.Interval != nil {
			interval = *sub.Interval
		}
		if replaced {
			util.LogInfo("[ADMIN] subscription updated in %s: %s", s.saveTarget, sub.Name)
		} else {
			util.LogInfo("[ADMIN] subscription added to %s: %s", s.saveTarget, sub.Name)
			w.WriteHeader(http.StatusCreated)
		}
		jsonResponse(w, map[string]interface{}{
			"name":      sub.Name,
			"url":       sub.URL,
			"interval":  interval,
			"nodeCount": len(sub.SubProxies),
		})

	case http.MethodDelete:
		dc := s.displayConf()
		name := strings.TrimPrefix(r.URL.Path, "/api/subscriptions/")
		if name == "" {
			httpError(w, "subscription name required", http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		for _, g := range dc.ProxyGroups {
			if g.Subscription == name {
				s.mu.Unlock()
				httpError(w, "subscription is referenced by group "+g.Name, http.StatusBadRequest)
				return
			}
		}

		for i, sub := range dc.Subscriptions {
			if sub.Name == name {
				old := dc.Subscriptions
				dc.Subscriptions = append(dc.Subscriptions[:i], dc.Subscriptions[i+1:]...)
				if err := dc.Init(); err != nil {
					dc.Subscriptions = old
					s.mu.Unlock()
					httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
					return
				}
				if err := s.saveConfigLocked(); err != nil {
					s.mu.Unlock()
					httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
					return
				}
				if err := s.mergeAndInitLocked(); err != nil {
					util.LogWarn("[ADMIN] merge after subscription delete failed: %v", err)
				}
				s.mu.Unlock()
				util.LogInfo("[ADMIN] subscription deleted from %s: %s", s.saveTarget, name)
				jsonResponse(w, map[string]string{"status": "deleted"})
				return
			}
		}
		s.mu.Unlock()
		httpError(w, "subscription not found", http.StatusNotFound)

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

		nodeCount := 0
		s.mu.RLock()
		conf := s.conf
		s.mu.RUnlock()
		if conf != nil {
			for _, sub := range conf.Subscriptions {
				if sub.Name == name {
					sub.SubMu.RLock()
					nodeCount = len(sub.SubProxies)
					sub.SubMu.RUnlock()
					break
				}
			}
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
		s.mu.RLock()
		conf := s.conf
		s.mu.RUnlock()
		if conf == nil {
			httpError(w, "config not ready", http.StatusServiceUnavailable)
			return
		}
		var sub *config.Subscription
		for _, ss := range conf.Subscriptions {
			if ss.Name == name {
				sub = ss
				break
			}
		}
		if sub == nil {
			httpError(w, "subscription not found", http.StatusNotFound)
			return
		}
		sub.SubMu.RLock()
		names := make([]string, 0, len(sub.SubProxies))
		for n := range sub.SubProxies {
			names = append(names, n)
		}
		sub.SubMu.RUnlock()
		sort.Strings(names)

		// Collect the latest health info from any runtime group that references
		// this subscription so the unified node viewer can show initial status.
		healthByNode := make(map[string]config.HealthInfo)
		for _, g := range conf.ProxyGroups {
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
			sub.SubMu.RLock()
			p := sub.SubProxies[n]
			sub.SubMu.RUnlock()
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
	dc := s.displayConf()
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
	for _, sub := range dc.Subscriptions {
		if sub.Name != name {
			continue
		}
		sub.Enabled = &body.Enabled
		if err := dc.Init(); err != nil {
			httpError(w, "config invalid: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.saveConfigLocked(); err != nil {
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.mergeAndInitLocked(); err != nil {
			util.LogWarn("[ADMIN] merge after subscription toggle failed: %v", err)
		}
		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after subscription toggle failed: %v", err)
			}
		}
		interval := 0
		if sub.Interval != nil {
			interval = *sub.Interval
		}
		jsonResponse(w, map[string]interface{}{
			"name":      sub.Name,
			"enabled":   sub.IsEnabled(),
			"url":       sub.URL,
			"interval":  interval,
			"nodeCount": len(sub.SubProxies),
		})
		return
	}
	httpError(w, "subscription not found", http.StatusNotFound)
}

func (s *AdminServer) apiHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()

	s.stats.CollectFromConfig(conf)
	snapshot := s.stats.GetSnapshot()
	jsonResponse(w, snapshot["healthData"])
}

// reverseConfigHelpers (used by apiReverse and apiReverseItem)

// currentReverseConfigs returns the active reverse config list, preferring the
// runtime config and falling back to the setup profile.
func (s *AdminServer) currentReverseConfigs() []*config.ReverseConfig {
	s.mu.RLock()
	conf := s.conf
	s.mu.RUnlock()
	if conf != nil && len(conf.ReverseConfigs) > 0 {
		return conf.ReverseConfigs
	}
	if profile, err := setup.LoadProfile(); err == nil && profile != nil {
		if profile.ReverseConfigs == nil {
			return []*config.ReverseConfig{}
		}
		return profile.ReverseConfigs
	}
	return []*config.ReverseConfig{}
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

// assignReverseID ensures rc.ReverseID is set and does not collide with any
// existing config. skipName allows updating an existing config without treating
// its old ReverseID as a collision.
func assignReverseID(rc *config.ReverseConfig, configs []*config.ReverseConfig, skipName string) {
	if rc.ReverseID == "" {
		if id, err := reverse.GenerateReverseID(); err == nil {
			rc.ReverseID = id
		}
	}
	for {
		if rc.ReverseID == "" {
			if id, err := reverse.GenerateReverseID(); err == nil {
				rc.ReverseID = id
			}
		}
		conflict := false
		for _, c := range configs {
			if c == nil || c.Name == skipName {
				continue
			}
			if c.ReverseID == rc.ReverseID {
				conflict = true
				break
			}
		}
		if !conflict {
			break
		}
		if id, err := reverse.GenerateReverseID(); err == nil {
			rc.ReverseID = id
		}
	}
}

// validateReverseConfig normalizes and validates a reverse config from the UI.
func (s *AdminServer) validateReverseConfig(rc *config.ReverseConfig) error {
	rc.Name = cleanReverseName(rc.Name)
	if rc.Name == "" {
		return fmt.Errorf("config name is required")
	}
	if rc.RegistryAddr == "" {
		return fmt.Errorf("registry address is required")
	}
	if rc.OutboundProxy == "" {
		return fmt.Errorf("outbound proxy is required")
	}
	if rc.RegisterProto == "" {
		rc.RegisterProto = "socks5"
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
		jsonResponse(w, reverseEventPayload(s.currentReverseConfigs()))

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
		assignReverseID(&rc, configs, "")

		profile, _ := setup.LoadProfile()
		if profile == nil {
			profile = &config.RuleConfiguration{}
		}
		profile.ReverseConfigs = append(profile.ReverseConfigs, &rc)
		if err := setup.SaveProfile(profile); err != nil {
			httpError(w, "save profile fail: "+err.Error(), http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		if s.conf == nil {
			s.conf = &config.RuleConfiguration{}
		}
		s.conf.ReverseConfigs = append(s.conf.ReverseConfigs, &rc)
		s.mu.Unlock()

		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after reverse config create failed: %v", err)
			}
		}
		util.LogInfo("[ADMIN] reverse config created: %s", rc.Name)
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

		profile, _ := setup.LoadProfile()
		if profile == nil {
			profile = &config.RuleConfiguration{}
		}

		var found *config.ReverseConfig
		for _, existing := range profile.ReverseConfigs {
			if existing != nil && existing.Name == name {
				found = existing
				break
			}
		}
		if found == nil {
			httpError(w, "reverse config not found", http.StatusNotFound)
			return
		}

		// Preserve the existing ReverseID unless the request explicitly changed it.
		// This keeps the registry binding stable across edits.
		if rc.ReverseID == "" {
			rc.ReverseID = found.ReverseID
		}
		assignReverseID(&rc, profile.ReverseConfigs, name)

		*found = rc
		if err := setup.SaveProfile(profile); err != nil {
			httpError(w, "save profile fail: "+err.Error(), http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		if s.conf == nil {
			s.conf = &config.RuleConfiguration{}
		}
		for i, existing := range s.conf.ReverseConfigs {
			if existing != nil && existing.Name == name {
				s.conf.ReverseConfigs[i] = found
				break
			}
		}
		s.mu.Unlock()

		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after reverse config update failed: %v", err)
			}
		}
		util.LogInfo("[ADMIN] reverse config updated: %s", rc.Name)
		jsonResponse(w, found)

	case http.MethodDelete:
		profile, _ := setup.LoadProfile()
		if profile == nil {
			httpError(w, "reverse config not found", http.StatusNotFound)
			return
		}

		var filtered []*config.ReverseConfig
		var removed bool
		for _, existing := range profile.ReverseConfigs {
			if existing != nil && existing.Name == name {
				removed = true
				continue
			}
			filtered = append(filtered, existing)
		}
		if !removed {
			httpError(w, "reverse config not found", http.StatusNotFound)
			return
		}
		profile.ReverseConfigs = filtered
		if err := setup.SaveProfile(profile); err != nil {
			httpError(w, "save profile fail: "+err.Error(), http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		if s.conf != nil {
			var filteredRuntime []*config.ReverseConfig
			for _, existing := range s.conf.ReverseConfigs {
				if existing != nil && existing.Name == name {
					continue
				}
				filteredRuntime = append(filteredRuntime, existing)
			}
			s.conf.ReverseConfigs = filteredRuntime
		}
		s.mu.Unlock()

		if s.OnIncrementalUpdate != nil {
			if err := s.OnIncrementalUpdate(); err != nil {
				util.LogWarn("[ADMIN] incremental update after reverse config delete failed: %v", err)
			}
		}
		util.LogInfo("[ADMIN] reverse config deleted: %s", name)
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

	profile, _ := setup.LoadProfile()
	if profile == nil {
		profile = &config.RuleConfiguration{}
	}

	var found *config.ReverseConfig
	for _, existing := range profile.ReverseConfigs {
		if existing != nil && existing.Name == name {
			found = existing
			break
		}
	}
	if found == nil {
		httpError(w, "reverse config not found", http.StatusNotFound)
		return
	}

	found.Enabled = req.Enabled
	if err := setup.SaveProfile(profile); err != nil {
		httpError(w, "save profile fail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	if s.conf == nil {
		s.conf = &config.RuleConfiguration{}
	}
	for _, existing := range s.conf.ReverseConfigs {
		if existing != nil && existing.Name == name {
			existing.Enabled = req.Enabled
			break
		}
	}
	s.mu.Unlock()

	if s.OnIncrementalUpdate != nil {
		if err := s.OnIncrementalUpdate(); err != nil {
			util.LogWarn("[ADMIN] incremental update after reverse config toggle failed: %v", err)
		}
	}
	util.LogInfo("[ADMIN] reverse config %s toggled: enabled=%v", name, req.Enabled)
	jsonResponse(w, found)
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

func (s *AdminServer) apiTUN(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		status := map[string]interface{}{
			"available":  tun.Available(),
			"enabled":    true,
			"running":    false,
			"deviceName": "",
			"routes":     tun.RouteSnapshot{},
			"logs":       []string{},
		}
		if s.GetTUNStatus != nil {
			if runtime := s.GetTUNStatus(); runtime != nil {
				for k, v := range runtime {
					status[k] = v
				}
			}
		} else {
			// Fallback: derive enabled from the config being edited
			dc := s.displayConf()
			if dc.TUN != nil && dc.TUN.Enabled != nil {
				status["enabled"] = *dc.TUN.Enabled
			}
		}
		jsonResponse(w, status)

	case http.MethodPatch:
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail", http.StatusBadRequest)
			return
		}
		if s.OnTUNToggle == nil {
			httpError(w, "TUN toggle not available", http.StatusServiceUnavailable)
			return
		}

		// Resolve the editable config outside the lock; displayConf uses RLock
		// and must not be called while holding the write lock.
		dc := s.displayConf()

		if err := s.OnTUNToggle(req.Enabled); err != nil {
			httpError(w, err.Error(), http.StatusInternalServerError)
			return
		}

		s.mu.Lock()
		if dc.TUN == nil {
			dc.TUN = &config.TUNConfig{}
		}
		dc.TUN.Enabled = &req.Enabled
		// Keep runtime config in sync so the status API reports the new state immediately.
		if s.conf != nil {
			if s.conf.TUN == nil {
				s.conf.TUN = &config.TUNConfig{}
			}
			s.conf.TUN.Enabled = &req.Enabled
		}
		if err := s.saveConfigLocked(); err != nil {
			s.mu.Unlock()
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.mu.Unlock()

		util.LogInfo("[ADMIN] TUN toggle enabled=%v", req.Enabled)
		util.DefaultVersionNotifier.BumpVersion("tun")
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
		SameSite: http.SameSiteStrictMode,
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
		SameSite: http.SameSiteStrictMode,
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
	// Sync to the runtime config so saveConfig() persists admin settings
	if s.conf != nil {
		if s.conf.Admin == nil {
			s.conf.Admin = &config.AdminConfig{}
		}
		s.conf.Admin.AuthEnabled = s.config.AuthEnabled
		s.conf.Admin.Username = s.config.Username
		s.conf.Admin.Password = s.config.Password
	}
	if s.baseConf != nil {
		if s.baseConf.Admin == nil {
			s.baseConf.Admin = &config.AdminConfig{}
		}
		s.baseConf.Admin.AuthEnabled = s.config.AuthEnabled
		s.baseConf.Admin.Username = s.config.Username
		s.baseConf.Admin.Password = s.config.Password
	}
	s.mu.Unlock()

	// Save to config file
	if err := s.saveConfig(); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// If enabling auth and credentials were just set, create session immediately
	if req.AuthEnabled && req.Username != "" && req.Password != "" {
		expires := time.Now().Add(sessionDuration).Unix()
		session := s.signSession(req.Username, expires)
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    session,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Expires:  time.Now().Add(sessionDuration),
		})
	}

	util.LogInfo("[ADMIN] auth settings updated")
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
	// Sync to runtime config so saveConfig() persists admin settings
	if s.conf != nil {
		if s.conf.Admin == nil {
			s.conf.Admin = &config.AdminConfig{}
		}
		s.conf.Admin.AuthEnabled = s.config.AuthEnabled
		s.conf.Admin.Username = s.config.Username
		s.conf.Admin.Password = s.config.Password
	}
	if s.baseConf != nil {
		if s.baseConf.Admin == nil {
			s.baseConf.Admin = &config.AdminConfig{}
		}
		s.baseConf.Admin.AuthEnabled = s.config.AuthEnabled
		s.baseConf.Admin.Username = s.config.Username
		s.baseConf.Admin.Password = s.config.Password
	}
	s.mu.Unlock()

	// Save reverse configs to profile and runtime (setup wizard always replaces the list)
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
			assignReverseID(rc, append(existing, req.ReverseConfigs...), rc.Name)
		}

		profile := &config.RuleConfiguration{ReverseConfigs: req.ReverseConfigs}
		if err := setup.SaveProfile(profile); err != nil {
			httpError(w, "save profile fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Update runtime
		s.mu.Lock()
		if s.conf == nil {
			s.conf = &config.RuleConfiguration{}
		}
		s.conf.ReverseConfigs = req.ReverseConfigs
		s.mu.Unlock()
	}

	// Save config
	if err := s.saveConfig(); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Create session if auth enabled
	if req.AuthEnabled && req.Username != "" {
		expires := time.Now().Add(sessionDuration).Unix()
		session := s.signSession(req.Username, expires)
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    session,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Expires:  time.Now().Add(sessionDuration),
		})
	}

	// Trigger reload
	if s.OnReload != nil {
		s.OnReload()
	}

	util.LogInfo("[ADMIN] setup wizard completed")
	jsonResponse(w, map[string]string{"status": "ok"})
}

// ========== Helpers ==========

func (s *AdminServer) render(w http.ResponseWriter, r *http.Request, pageName string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	// Ensure every page has a non-empty Version for static-file cache busting.
	if m, ok := data.(map[string]interface{}); ok {
		if v, _ := m["Version"].(string); v == "" {
			m["Version"] = adminVersion
		}
	}

	var t *template.Template
	switch pageName {
	case "dashboard.html":
		t = s.pages.dashboard
	case "proxies.html":
		t = s.pages.proxies
	case "subscriptions.html":
		t = s.pages.subscriptions
	case "rules.html":
		t = s.pages.rules
	case "mappings.html":
		t = s.pages.mappings
	case "reverse-wizard.html":
		t = s.pages.reverseWizard
	case "config.html":
		t = s.pages.config
	case "login.html":
		t = s.pages.login
	case "setup.html":
		t = s.pages.setup
	default:
		util.LogError("[ADMIN] unknown page: %s", pageName)
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		if err := t.ExecuteTemplate(w, "content", data); err != nil {
			util.LogError("[ADMIN] fragment template error (%s): %v", pageName, err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
		return
	}

	if err := t.Execute(w, data); err != nil {
		util.LogError("[ADMIN] template error (%s): %v", pageName, err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

// saveConfig persists the current configuration to disk and triggers a reload.
// saveConfig persists the current configuration to disk.
// It acquires its own read lock and is safe to call from handlers
// that do not already hold the lock.
func (s *AdminServer) saveConfig() error {
	s.mu.RLock()
	conf := s.conf
	path := s.confPath
	s.mu.RUnlock()

	// If path is a directory, append rule.yaml
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "rule.yaml")
	}

	if err := config.SaveRaw(path, conf); err != nil {
		return err
	}
	util.LogInfo("[ADMIN] config saved to %s", path)
	return nil
}

// saveConfigLocked persists the currently-edited config (baseConf or envConf) to disk.
// Caller must hold s.mu (write lock).
func (s *AdminServer) saveConfigLocked() error {
	target := s.confPath
	conf := s.baseConf
	if s.saveTarget == "env" && s.envPath != "" && s.envConf != nil {
		target = s.envPath
		conf = s.envConf
	}
	// If target is a directory, append rule.yaml
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, "rule.yaml")
	}
	if err := config.SaveRaw(target, conf); err != nil {
		return err
	}
	util.LogInfo("[ADMIN] config saved to %s (target=%s)", target, s.saveTarget)
	util.DefaultVersionNotifier.BumpVersion("config")
	return nil
}

func sanitizeConfig(conf *config.RuleConfiguration) map[string]interface{} {
	proxies := make([]map[string]interface{}, len(conf.Proxies))
	for i, p := range conf.Proxies {
		proxies[i] = map[string]interface{}{
			"name":      p.Name,
			"type":      p.Type,
			"server":    p.Server,
			"port":      p.Port,
			"sni":       p.Sni,
			"udp":       p.UDP,
			"proxyName": p.ProxyName,
		}
	}

	result := map[string]interface{}{
		"proxies": proxies,
		"rules":   conf.Rules,
		"mappings": func() []map[string]interface{} {
			result := make([]map[string]interface{}, len(conf.Mappings))
			for i, m := range conf.Mappings {
				result[i] = map[string]interface{}{
					"name": m.Name,
					"type": m.Type,
					"port": m.Port,
				}
			}
			return result
		}(),
	}
	if conf.Admin != nil {
		result["admin"] = map[string]interface{}{
			"auth-enabled": conf.Admin.AuthEnabled,
			"username":     conf.Admin.Username,
		}
	}
	return result
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
		"name":           p.Name,
		"enabled":        p.IsEnabled(),
		"type":           p.Type,
		"server":         p.Server,
		"port":           p.Port,
		"sni":            p.Sni,
		"udp":            p.UDP,
		"proxyName":      p.ProxyName,
		"skipCertVerify": p.SkipCertVerify,
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

// UpdateConfig allows external callers (e.g., main.go reload) to update the config reference.
func (s *AdminServer) UpdateConfig(conf *config.RuleConfiguration) {
	s.mu.Lock()
	s.conf = conf
	s.mu.Unlock()
}

// GetConfig returns current config reference (for read-only access).
func (s *AdminServer) GetConfig() *config.RuleConfiguration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conf
}
