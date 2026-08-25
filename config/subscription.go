package config

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FetchSubscription downloads subscription content from URL.
// If dialFunc is provided, HTTP requests will go through the proxy.
func FetchSubscription(subURL string, dialFunc func(network, addr string) (net.Conn, error)) (string, error) {
	var client *http.Client
	if dialFunc != nil {
		transport := &http.Transport{Dial: dialFunc}
		client = &http.Client{Timeout: 30 * time.Second, Transport: transport}
	} else {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequest("GET", subURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ClashForWindows/0.20.39")
	req.Header.Set("Accept", "*/*")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("subscription: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ParseSubscription parses subscription content into Proxy list.
// Supports:
//   - Base64-encoded URI list (most common)
//   - Plain URI list (one per line)
//   - Clash YAML format
func ParseSubscription(content string) ([]*Proxy, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, nil
	}

	// Try base64 decode first
	decoded := content
	if d, err := tryBase64Decode(content); err == nil {
		decoded = d
	}

	var proxies []*Proxy
	var err error

	// Try Clash YAML format
	if strings.Contains(decoded, "proxies:") {
		proxies, err = parseClashSubscription(decoded)
		if err != nil {
			// Fallback to URI list if YAML parse fails
			proxies, err = parseURIList(decoded)
		}
	} else {
		proxies, err = parseURIList(decoded)
	}
	if err != nil {
		return nil, err
	}

	// Post-process: normalize VLESS UUIDs and fill missing SNI/skipVerify
	normalizeVLESSUUIDs(proxies)
	fillVLESSMissingSNI(proxies)
	return proxies, nil
}

// normalizeVLESSUUIDs converts non-standard UUIDs (like subscription tokens)
// to standard 32-hex UUIDs that xray-core accepts.
func normalizeVLESSUUIDs(proxies []*Proxy) {
	for _, p := range proxies {
		if p.Type != ProxyVLESS {
			continue
		}
		p.UUID = parseUUID(p.UUID)
	}
}

// parseUUID converts any string to a valid UUID.
// If already a standard UUID, returns as-is.
// Otherwise uses MD5 hash to derive a 16-byte UUID.
func parseUUID(s string) string {
	if s == "" {
		return s
	}
	// Remove common separators
	clean := strings.ReplaceAll(s, "-", "")
	clean = strings.ReplaceAll(clean, " ", "")

	// Check if it's already a valid hex UUID (32 or 36 chars with dashes)
	if len(clean) == 32 {
		if _, err := hex.DecodeString(clean); err == nil {
			return s // already valid
		}
	}

	// Try base64 decode (some UUIDs are base64-encoded 16 bytes)
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && len(decoded) == 16 {
		return hex.EncodeToString(decoded)
	}
	if decoded, err := base64.URLEncoding.DecodeString(s); err == nil && len(decoded) == 16 {
		return hex.EncodeToString(decoded)
	}

	// Fallback: derive UUID from MD5 hash of the string
	hash := md5.Sum([]byte(s))
	return hex.EncodeToString(hash[:])
}

func fillVLESSMissingSNI(proxies []*Proxy) {
	// First: copy servername to sni for VLESS nodes if sni is empty
	for _, p := range proxies {
		if p.Type == ProxyVLESS && p.Sni == "" && p.Servername != "" {
			p.Sni = p.Servername
		}
	}

	serverInfo := make(map[string]struct {
		sni        string
		skipVerify bool
	})
	for _, p := range proxies {
		if p.Type == ProxyVLESS {
			continue
		}
		key := fmt.Sprintf("%s:%d", p.Server, p.Port)
		if p.Sni != "" {
			info := serverInfo[key]
			info.sni = p.Sni
			info.skipVerify = info.skipVerify || p.SkipCertVerify
			serverInfo[key] = info
		}
	}
	serverOnlyInfo := make(map[string]struct {
		sni        string
		skipVerify bool
	})
	for _, p := range proxies {
		if p.Type == ProxyVLESS {
			continue
		}
		if p.Sni != "" {
			info := serverOnlyInfo[p.Server]
			if info.sni == "" {
				info.sni = p.Sni
			}
			info.skipVerify = info.skipVerify || p.SkipCertVerify
			serverOnlyInfo[p.Server] = info
		}
	}
	for _, p := range proxies {
		if p.Type != ProxyVLESS {
			continue
		}
		if p.Sni != "" {
			continue
		}
		key := fmt.Sprintf("%s:%d", p.Server, p.Port)
		if info, ok := serverInfo[key]; ok && info.sni != "" {
			p.Sni = info.sni
			if info.skipVerify {
				p.SkipCertVerify = true
			}
			continue
		}
		if info, ok := serverOnlyInfo[p.Server]; ok && info.sni != "" {
			p.Sni = info.sni
			if info.skipVerify {
				p.SkipCertVerify = true
			}
		}
	}
}

func tryBase64Decode(s string) (string, error) {
	// Pad if needed
	padding := len(s) % 4
	if padding > 0 {
		s += strings.Repeat("=", 4-padding)
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func parseURIList(content string) ([]*Proxy, error) {
	var proxies []*Proxy
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, err := parseURI(line)
		if err != nil {
			continue // skip invalid lines
		}
		if p != nil {
			proxies = append(proxies, p)
		}
	}
	return proxies, nil
}

func parseURI(uri string) (*Proxy, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	name := u.Fragment
	if name == "" {
		name = u.Host
	}
	// URL decode name
	if decoded, err := url.QueryUnescape(name); err == nil {
		name = decoded
	}

	host := u.Hostname()
	port, _ := strconv.Atoi(u.Port())

	switch u.Scheme {
	case "ss":
		return parseSSURI(u, name, host, port)
	case "trojan":
		return parseTrojanURI(u, name, host, port)
	case "hysteria", "hysteria2", "hy2":
		return parseHysteria2URI(u, name, host, port)
	case "http", "https":
		return parseHTTPURI(u, name, host, port)
	case "socks5":
		return parseSocks5URI(u, name, host, port)
	case "vless":
		return parseVLESSURI(u, name, host, port)
	default:
		return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
}

// ss://BASE64(method:password)@server:port#name
// ss://BASE64(method:password@server:port)#name
func parseSSURI(u *url.URL, name, host string, port int) (*Proxy, error) {
	var method, password string

	if u.User != nil {
		// ss://BASE64(method:password)@server:port#name
		encoded := u.User.Username()
		decoded, err := tryBase64Decode(encoded)
		if err != nil {
			return nil, err
		}
		parts := strings.SplitN(decoded, ":", 2)
		if len(parts) == 2 {
			method = parts[0]
			password = parts[1]
		}
	} else {
		// ss://BASE64(method:password@server:port)#name
		encoded := strings.TrimPrefix(u.String(), "ss://")
		if idx := strings.Index(encoded, "#"); idx != -1 {
			encoded = encoded[:idx]
		}
		decoded, err := tryBase64Decode(encoded)
		if err != nil {
			return nil, err
		}
		// decoded: method:password@server:port
		atIdx := strings.LastIndex(decoded, "@")
		if atIdx == -1 {
			return nil, fmt.Errorf("invalid ss uri")
		}
		mp := decoded[:atIdx]
		hp := decoded[atIdx+1:]
		parts := strings.SplitN(mp, ":", 2)
		if len(parts) == 2 {
			method = parts[0]
			password = parts[1]
		}
		hostPort := strings.SplitN(hp, ":", 2)
		if len(hostPort) == 2 {
			host = hostPort[0]
			port, _ = strconv.Atoi(hostPort[1])
		}
	}

	return &Proxy{
		Name:     name,
		Type:     "ss", // ss maps to shadowsocks
		Server:   host,
		Port:     port,
		Password: password,
		Cipher:   method,
	}, nil
}

// trojan://password@server:port?sni=xxx&allowInsecure=1#name
func parseTrojanURI(u *url.URL, name, host string, port int) (*Proxy, error) {
	password := u.User.Username()

	q := u.Query()
	p := &Proxy{
		Name:     name,
		Type:     ProxyTROJAN,
		Server:   host,
		Port:     port,
		Password: password,
		Sni:      q.Get("sni"),
	}
	if q.Get("allowInsecure") == "1" || q.Get("skip-cert-verify") == "true" {
		p.SkipCertVerify = true
	}
	if q.Get("udp") == "true" || q.Get("udp") == "1" {
		p.UDP = true
	}
	return p, nil
}

// hysteria2://password@server:port?sni=xxx&insecure=1#name
func parseHysteria2URI(u *url.URL, name, host string, port int) (*Proxy, error) {
	password := u.User.Username()

	q := u.Query()
	p := &Proxy{
		Name:     name,
		Type:     ProxyHYSTERIA2,
		Server:   host,
		Port:     port,
		Password: password,
		Sni:      q.Get("sni"),
	}
	if q.Get("insecure") == "1" || q.Get("skip-cert-verify") == "true" {
		p.SkipCertVerify = true
	}
	if q.Get("udp") == "true" || q.Get("udp") == "1" {
		p.UDP = true
	}
	return p, nil
}

// http://username:password@server:port#name
func parseHTTPURI(u *url.URL, name, host string, port int) (*Proxy, error) {
	p := &Proxy{
		Name:   name,
		Type:   ProxyHTTP,
		Server: host,
		Port:   port,
	}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	return p, nil
}

// socks5://username:password@server:port#name
func parseSocks5URI(u *url.URL, name, host string, port int) (*Proxy, error) {
	p := &Proxy{
		Name:   name,
		Type:   ProxySOCKS5,
		Server: host,
		Port:   port,
	}
	if u.User != nil {
		p.Username = u.User.Username()
		p.Password, _ = u.User.Password()
	}
	return p, nil
}

// vless://uuid@server:port?encryption=none&sni=xxx&...#name
func parseVLESSURI(u *url.URL, name, host string, port int) (*Proxy, error) {
	uuid := u.User.Username()

	q := u.Query()
	p := &Proxy{
		Name:     name,
		Type:     ProxyVLESS,
		Server:   host,
		Port:     port,
		Password: uuid,
		UUID:     uuid,
		Sni:      q.Get("sni"),
	}
	// Always parse skip-cert-verify regardless of security type
	if q.Get("allowInsecure") == "1" || q.Get("skip-cert-verify") == "true" || q.Get("insecure") == "1" {
		p.SkipCertVerify = true
	}
	if q.Get("flow") != "" {
		// flow control, store in URL for now
		p.URL = q.Get("flow")
	}
	p.Cipher = q.Get("security") // borrow Cipher field to store security type for logging
	return p, nil
}

// parseClashSubscription parses Clash YAML subscription format
func parseClashSubscription(content string) ([]*Proxy, error) {
	type clashSub struct {
		Proxies []*Proxy `yaml:"proxies"`
	}
	var sub clashSub
	if err := yaml.Unmarshal([]byte(content), &sub); err != nil {
		return nil, err
	}
	return sub.Proxies, nil
}

// FetchSubscriptionCached fetches subscription content, falling back to disk cache
// on failure. On success, the content is saved to cache for future restarts.
// Returns (content, fromCache, error).
func FetchSubscriptionCached(subURL string, dialFunc func(network, addr string) (net.Conn, error), cacheDir, groupName string) (string, bool, error) {
	// 1. Try network fetch
	content, err := FetchSubscription(subURL, dialFunc)
	if err == nil {
		// Save to cache (best-effort, don't fail on cache write error)
		_ = SaveSubscriptionCache(cacheDir, groupName, content)
		return content, false, nil
	}

	// 2. Network fetch failed, try cache
	if cached, cacheErr := LoadSubscriptionCache(cacheDir, groupName); cacheErr == nil && cached != "" {
		return cached, true, nil
	}

	// 3. Both failed
	return "", false, err
}

// ========== Subscription Cache ==========

// cacheFileName returns a safe filename for a group's subscription cache.
// Uses the group name sanitized for filesystem use.
func cacheFileName(groupName string) string {
	// Replace path separators and other problematic chars
	safe := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, groupName)
	return safe + ".cache"
}

// SaveSubscriptionCache writes raw subscription content to disk.
func SaveSubscriptionCache(cacheDir, groupName, content string) error {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}
	path := filepath.Join(cacheDir, cacheFileName(groupName))
	return os.WriteFile(path, []byte(content), 0644)
}

// LoadSubscriptionCache reads cached subscription content from disk.
// Returns ("", nil) if no cache exists.
func LoadSubscriptionCache(cacheDir, groupName string) (string, error) {
	path := filepath.Join(cacheDir, cacheFileName(groupName))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}
