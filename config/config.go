package config

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

// ========== Proxy ==========

const (
	ProxyDIRECT    = "DIRECT"
	ProxyREJECT    = "REJECT"
	ProxySERVER    = "SERVER"
	ProxyTROJAN    = "TROJAN"
	ProxySOCKS5    = "SOCKS5"
	ProxyH_TUNNEL  = "H_TUNNEL"
	ProxyHYSTERIA2 = "HYSTERIA2"
	ProxyREVERSE   = "REVERSE"
	ProxyHTTP      = "HTTP"
	ProxyHTTPS     = "HTTPS"
	ProxyVLESS     = "VLESS"
	ProxySSH       = "SSH"
)

type Proxy struct {
	Name                 string `yaml:"name" json:"name"`
	Enabled              *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Type                 string `yaml:"type" json:"type"`
	Server               string `yaml:"server" json:"server,omitempty"`
	Port                 int    `yaml:"port" json:"port,omitempty"`
	Username             string `yaml:"username,omitempty" json:"username,omitempty"`
	Password             string `yaml:"password,omitempty" json:"password,omitempty"`
	PrivateKey           string `yaml:"private-key,omitempty" json:"private-key,omitempty"`
	PrivateKeyPassphrase string `yaml:"private-key-passphrase,omitempty" json:"private-key-passphrase,omitempty"`
	UUID                 string `yaml:"uuid,omitempty" json:"uuid,omitempty"`
	Sni                  string `yaml:"sni,omitempty" json:"sni,omitempty"`
	Servername           string `yaml:"servername,omitempty" json:"servername,omitempty"` // VLESS REALITY uses servername instead of sni
	SkipCertVerify       bool   `yaml:"skip-cert-verify,omitempty" json:"skip-cert-verify,omitempty"`
	UDP                  bool   `yaml:"udp,omitempty" json:"udp,omitempty"`
	Cipher               string `yaml:"cipher,omitempty" json:"cipher,omitempty"`
	Tfo                  bool   `yaml:"tfo,omitempty" json:"tfo,omitempty"`
	URL                  string `yaml:"url,omitempty" json:"url,omitempty"`
	ViaProxy             string `yaml:"via,omitempty" json:"via,omitempty"` // 通过哪个代理建立连接
	HealthCheckURL       string `yaml:"health-check-url,omitempty" json:"health-check-url,omitempty"` // 健康检查 URL，配置后会通过代理发送实际请求验证
	ReverseAddress       string `yaml:"reverse-address,omitempty" json:"reverse-address,omitempty"`
	UpBps                int64  `yaml:"up-bps,omitempty" json:"up-bps,omitempty"`
	DownBps              int64  `yaml:"down-bps,omitempty" json:"down-bps,omitempty"`
	// Up/Down are the Clash/Mihomo shorthand bandwidth fields (in Mbps).
	// They are converted to UpBps/DownBps (bytes per second) during Init.
	Up   int64 `yaml:"up,omitempty" json:"up,omitempty"`
	Down int64 `yaml:"down,omitempty" json:"down,omitempty"`

	// VLESS/XTLS/REALITY fields
	Flow        string            `yaml:"flow,omitempty" json:"flow,omitempty"`
	Fingerprint string            `yaml:"client-fingerprint,omitempty" json:"client-fingerprint,omitempty"`
	RealityOpts map[string]string `yaml:"reality-opts,omitempty" json:"reality-opts,omitempty"`

	SourceGroup     string       `yaml:"-" json:"-"` // 来源订阅组名，手动配置为空
	Next            *Proxy       `yaml:"-" json:"-"`
	UpRateLimiter   *RateLimiter `yaml:"-" json:"-"`
	DownRateLimiter *RateLimiter `yaml:"-" json:"-"`
}

// IsEnabled reports whether the proxy is enabled. Omitted or nil means enabled.
func (p *Proxy) IsEnabled() bool {
	if p == nil || p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

func SingletonProxy(typ string) *Proxy {
	return &Proxy{Type: typ}
}

// ========== Mapping ==========

type Mapping struct {
	Name                  string `yaml:"name" json:"name"`
	Enabled               *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Type                  string `yaml:"type" json:"type"`
	Port                  int    `yaml:"port" json:"port"`
	ReverseAddress        string `yaml:"reverse-address,omitempty" json:"reverse-address,omitempty"`
	ReverseProxy          string `yaml:"reverse-proxy,omitempty" json:"reverse-proxy,omitempty"`
	ReverseMaxConnections int    `yaml:"reverse-max-connections,omitempty" json:"reverse-max-connections,omitempty"`
	ReverseRetryInterval  int64  `yaml:"reverse-retry-interval,omitempty" json:"reverse-retry-interval,omitempty"`
	DstHost               string `yaml:"dst-host,omitempty" json:"dst-host,omitempty"`
	DstPort               int    `yaml:"dst-port,omitempty" json:"dst-port,omitempty"`
	Username              string `yaml:"username,omitempty" json:"username,omitempty"`
	Password              string `yaml:"password,omitempty" json:"password,omitempty"`
	Sni                   string `yaml:"sni,omitempty" json:"sni,omitempty"`
}

// IsEnabled reports whether the mapping is enabled. Omitted or nil means enabled.
func (m *Mapping) IsEnabled() bool {
	if m == nil || m.Enabled == nil {
		return true
	}
	return *m.Enabled
}

// ========== ProxyGroup ==========

const (
	GroupSelect      = "select"
	GroupLoadBalance = "load-balance"
	GroupBest        = "best"
)

// Health-check sampling thresholds.
// A proxy is marked dead only after consecutive failures,
// and marked alive only after consecutive successes.
const (
	healthFailThreshold    = 3
	healthSuccessThreshold = 2
)

type healthStatus struct {
	latency      time.Duration
	alive        bool
	failCount    int
	successCount int
	lastCheck    time.Time
}

type Subscription struct {
	Name     string `yaml:"name" json:"name"`
	Enabled  *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	URL      string `yaml:"url" json:"url"`
	Interval *int   `yaml:"interval" json:"interval,omitempty"` // seconds

	// SubProxies is the parsed node pool. It is runtime-only and not persisted.
	SubProxies map[string]*Proxy `yaml:"-" json:"-"`
	SubMu      sync.RWMutex      `yaml:"-" json:"-"`
}

// SetNodes replaces the subscription node pool and sets SourceGroup to the
// subscription name so health checks can distinguish subscription nodes from
// manual proxies.
func (s *Subscription) SetNodes(nodes []*Proxy) {
	s.SubMu.Lock()
	defer s.SubMu.Unlock()
	s.SubProxies = make(map[string]*Proxy)
	for _, p := range nodes {
		if p.Name == "" {
			p.Name = fmt.Sprintf("%s:%d", p.Server, p.Port)
		}
		p.SourceGroup = s.Name
		s.SubProxies[p.Name] = p
	}
}

// IsEnabled reports whether the subscription is enabled. Omitted or nil means enabled.
func (s *Subscription) IsEnabled() bool {
	if s == nil || s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// GroupMember identifies a single member of a ProxyGroup and its source.
// Manual members reference the global proxy namespace or another proxy group;
// subscription members reference the group-local subscription node pool. Keeping
// the source lets a manual proxy and a subscription node share the same name
// without shadowing each other inside the same group.
type GroupMember struct {
	Name             string
	FromSubscription bool // false = manual global proxy or group, true = selected subscription node
	IsGroup          bool // true = manual member is a nested proxy group
}

// HealthKey returns the key used in the group's health map. Subscription
// members are prefixed so that a subscription node and a manual proxy with the
// same name can have independent health states.
func (m GroupMember) HealthKey() string {
	if m.FromSubscription {
		return "sub:" + m.Name
	}
	return m.Name
}

type ProxyGroup struct {
	Name                string   `yaml:"name" json:"name"`
	Enabled             *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Type                string   `yaml:"type" json:"type"`
	Proxies             []string `yaml:"proxies" json:"proxies"`           // runtime flat names derived from Members (may contain duplicates when a name exists in both sources)
	ManualProxies       []string `yaml:"-" json:"manualProxies,omitempty"` // manual proxy names copied from YAML
	HealthCheckURL      string   `yaml:"health-check-url" json:"health-check-url,omitempty"`
	HealthCheckInterval *int     `yaml:"health-check-interval" json:"health-check-interval,omitempty"`
	HealthCheckTolerance *int    `yaml:"health-check-tolerance,omitempty" json:"health-check-tolerance,omitempty"` // best type: switch if latency difference exceeds this (ms)
	LBStrategy          string   `yaml:"lb-strategy,omitempty" json:"lb-strategy,omitempty"`                     // load-balance: round-robin (default) or consistent-hashing

	// Subscription names the subscription provider this group draws nodes from.
	// Selection and filter are per-group, so multiple groups can reference the
	// same subscription and choose different nodes.
	Subscription         string   `yaml:"subscription,omitempty" json:"subscription,omitempty"`
	SubscriptionFilter   string   `yaml:"subscription-filter,omitempty" json:"subscription-filter,omitempty"`
	SubscriptionSelected []string `yaml:"subscription-selected,omitempty" json:"subscription-selected,omitempty"` // deprecated: migrated to ActiveMember on Init
	ActiveMember         string   `yaml:"active-member,omitempty" json:"active-member,omitempty"`
	SubscriptionMode     string   `yaml:"subscription-mode,omitempty" json:"subscription-mode,omitempty"` // deprecated: ignored

	// Members is the runtime ordered member list: selected subscription nodes
	// first, then manual members. It carries source information so manual and
	// subscription nodes with identical names can coexist in the same group.
	Members []GroupMember `yaml:"-" json:"members,omitempty"`

	// ManualMembers and SubMembers are the same members split by source.
	// Next() uses Members so that both sources participate in selection;
	// subscription nodes are tried first and manual proxies serve as fallback.
	ManualMembers []GroupMember `yaml:"-" json:"-"`
	SubMembers    []GroupMember `yaml:"-" json:"-"`

	// SubCandidateCount is a runtime display hint set by the admin UI. It holds
	// the number of subscription nodes that match the group's filter so the card
	// button count matches the nodes shown in the selection modal.
	SubCandidateCount int `yaml:"-" json:"-"`

	idx       int64 // for load-balance
	healthMap map[string]*healthStatus
	healthMu  sync.RWMutex
	selMu     sync.RWMutex `yaml:"-" json:"-"` // protects Members rebuild

	resolveProxy        func(name string) *Proxy        `yaml:"-" json:"-"`
	resolveGroup        func(name string) *ProxyGroup   `yaml:"-" json:"-"`
	resolveSubscription func(name string) *Subscription `yaml:"-" json:"-"`
}

// IsEnabled reports whether the group is enabled. Omitted or nil means enabled.
func (g *ProxyGroup) IsEnabled() bool {
	if g == nil || g.Enabled == nil {
		return true
	}
	return *g.Enabled
}

func (g *ProxyGroup) Init() error {
	switch g.Type {
	case GroupSelect, GroupLoadBalance, GroupBest:
		// ok
	default:
		return fmt.Errorf("unsupported group type: %s", g.Type)
	}

	// subscription-mode is deprecated and ignored.
	g.SubscriptionMode = ""
	// Migrate the old subscription-selected list to the single active-member
	// field used by select groups. Only the first entry is kept; membership is
	// now determined by subscription-filter.
	if g.ActiveMember == "" && len(g.SubscriptionSelected) > 0 {
		g.ActiveMember = g.SubscriptionSelected[0]
	}
	g.SubscriptionSelected = nil

	if len(g.ManualProxies) == 0 && len(g.Proxies) > 0 {
		// Both subscription and non-subscription groups store manual proxy
		// names in YAML under the `proxies` key. Copy them once so runtime
		// rebuilds can append selected subscription nodes without mutating
		// the persisted list.
		g.ManualProxies = make([]string, len(g.Proxies))
		copy(g.ManualProxies, g.Proxies)
	}
	g.healthMu.Lock()
	if g.healthMap == nil {
		g.healthMap = make(map[string]*healthStatus)
	}
	g.healthMu.Unlock()
	return nil
}

// Resolve looks up a proxy name for this group. Manual members are resolved in
// the global proxy namespace; selected subscription nodes are resolved in the
// group-local subscription pool. This keeps subscription nodes from leaking
// into the global proxy namespace and avoids collisions with manual proxies.
func (g *ProxyGroup) Resolve(name string) *Proxy {
	if name == "" {
		return nil
	}
	// Manual members of this group always resolve via the global proxy list.
	for _, m := range g.ManualProxies {
		if m == name {
			if g.resolveProxy != nil {
				return g.resolveProxy(name)
			}
			return nil
		}
	}
	// Otherwise, if the group references a subscription, resolve in its node pool.
	if g.resolveSubscription != nil && g.Subscription != "" {
		if sub := g.resolveSubscription(g.Subscription); sub != nil {
			sub.SubMu.RLock()
			if p, ok := sub.SubProxies[name]; ok {
				sub.SubMu.RUnlock()
				return p
			}
			sub.SubMu.RUnlock()
		}
	}
	// Fallback for direct global proxies that are not listed as manual members.
	if g.resolveProxy != nil {
		return g.resolveProxy(name)
	}
	return nil
}

// ResolveMember resolves a single group member according to its source.
// Group references (IsGroup) are not resolved here; Next()/NextWithVisited()
// handles nested groups so that a nil inner result can be skipped.
func (g *ProxyGroup) ResolveMember(m GroupMember) *Proxy {
	if m.IsGroup {
		return nil
	}
	if m.FromSubscription {
		if g.resolveSubscription != nil && g.Subscription != "" {
			if sub := g.resolveSubscription(g.Subscription); sub != nil {
				sub.SubMu.RLock()
				p := sub.SubProxies[m.Name]
				sub.SubMu.RUnlock()
				return p
			}
		}
		return nil
	}
	if g.resolveProxy != nil {
		return g.resolveProxy(m.Name)
	}
	return nil
}

// rebuildProxiesLocked rebuilds the runtime member lists from ManualProxies plus
// all subscription nodes that match SubscriptionFilter. g.Members keeps the full
// ordered list (subscription nodes first, then manual members) for display/health
// purposes, while g.ManualMembers and g.SubMembers split them by source.
// Subscription nodes are tried first so that manual proxies (e.g. DIRECT) act as
// fallback when all subscription nodes are unavailable.
// Names are NOT deduplicated so a manual proxy and a subscription node can share
// the same name and remain independent members. Caller must hold g.selMu.
func (g *ProxyGroup) rebuildProxiesLocked() {
	g.ManualMembers = make([]GroupMember, 0, len(g.ManualProxies))
	for _, name := range g.ManualProxies {
		// Manual members may be individual proxies or nested proxy groups.
		// Skip disabled or missing entries so groups stay valid when a proxy
		// is disabled without removing it from every group's manual list.
		if g.resolveProxy != nil && g.resolveProxy(name) != nil {
			g.ManualMembers = append(g.ManualMembers, GroupMember{Name: name, FromSubscription: false})
			continue
		}
		if g.resolveGroup != nil && g.resolveGroup(name) != nil {
			g.ManualMembers = append(g.ManualMembers, GroupMember{Name: name, FromSubscription: false, IsGroup: true})
			continue
		}
	}

	g.SubMembers = nil
	if g.Subscription != "" && g.resolveSubscription != nil {
		if sub := g.resolveSubscription(g.Subscription); sub != nil {
			names := g.subscriptionCandidatesLocked(sub)
			g.SubMembers = make([]GroupMember, 0, len(names))
			for _, name := range names {
				g.SubMembers = append(g.SubMembers, GroupMember{Name: name, FromSubscription: true})
			}
		}
	}

	g.Members = make([]GroupMember, 0, len(g.SubMembers)+len(g.ManualMembers))
	g.Members = append(g.Members, g.SubMembers...)
	g.Members = append(g.Members, g.ManualMembers...)
}

// hasSubscriptionSelection reports whether the group is subscription-driven.
// A group is considered subscription-driven when it references a subscription,
// regardless of how many nodes are currently selected.
func (g *ProxyGroup) hasSubscriptionSelection() bool {
	return g.Subscription != ""
}

// activeMembersLocked returns the members that participate in the group-type
// selection logic. Selected subscription nodes are listed first, followed by
// manual members. This lets a manual proxy (e.g. DIRECT) act as fallback when
// all subscription nodes are unavailable.
// Caller must hold g.selMu (read or write).
func (g *ProxyGroup) activeMembersLocked() []GroupMember {
	return g.Members
}

// RebuildProxies is the public, concurrency-safe wrapper.
func (g *ProxyGroup) RebuildProxies() {
	g.selMu.Lock()
	defer g.selMu.Unlock()
	g.rebuildProxiesLocked()
}

// GetMembers returns a snapshot of the current runtime member list.
func (g *ProxyGroup) GetMembers() []GroupMember {
	g.selMu.RLock()
	defer g.selMu.RUnlock()
	members := make([]GroupMember, len(g.Members))
	copy(members, g.Members)
	return members
}

// GetManualMembers returns a snapshot of the manual member list.
func (g *ProxyGroup) GetManualMembers() []GroupMember {
	g.selMu.RLock()
	defer g.selMu.RUnlock()
	members := make([]GroupMember, len(g.ManualMembers))
	copy(members, g.ManualMembers)
	return members
}

// GetActiveMember returns the currently configured active member for select
// groups. It may be empty if no explicit active member has been set.
func (g *ProxyGroup) GetActiveMember() string {
	g.selMu.RLock()
	defer g.selMu.RUnlock()
	return g.ActiveMember
}

// SetActiveMember sets the active member for a select group. The name must
// correspond to a current member; otherwise an error is returned. Rebuild is
// triggered so membership changes (e.g. from a filter update) are reflected.
func (g *ProxyGroup) SetActiveMember(name string) error {
	g.selMu.Lock()
	defer g.selMu.Unlock()
	if name == "" {
		g.ActiveMember = ""
		return nil
	}
	found := false
	for _, m := range g.Members {
		if m.Name == name {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("member %q not found in group %s", name, g.Name)
	}
	g.ActiveMember = name
	g.rebuildProxiesLocked()
	return nil
}

// SubscriptionCandidates returns the subscription node names that are available
// for this group, filtered by SubscriptionFilter. The filter is treated as a
// regular expression; if it fails to compile, it falls back to a case-insensitive
// substring match. The returned names are sorted.
func (g *ProxyGroup) SubscriptionCandidates() []string {
	if g.Subscription == "" || g.resolveSubscription == nil {
		return nil
	}
	sub := g.resolveSubscription(g.Subscription)
	if sub == nil {
		return nil
	}
	g.selMu.RLock()
	defer g.selMu.RUnlock()
	return g.subscriptionCandidatesLocked(sub)
}

// subscriptionCandidatesLocked returns the filtered, sorted candidate names from
// the provided subscription. Caller must hold g.selMu.
func (g *ProxyGroup) subscriptionCandidatesLocked(sub *Subscription) []string {
	var re *regexp.Regexp
	filter := strings.TrimSpace(g.SubscriptionFilter)
	if filter != "" {
		var err error
		re, err = regexp.Compile(filter)
		if err != nil {
			// Fall back to substring match on compile failure so a bad filter
			// does not break the group entirely.
			re = nil
		}
	}

	sub.SubMu.RLock()
	defer sub.SubMu.RUnlock()

	names := make([]string, 0, len(sub.SubProxies))
	for name := range sub.SubProxies {
		if filter == "" {
			names = append(names, name)
			continue
		}
		if re != nil {
			if re.MatchString(name) {
				names = append(names, name)
			}
		} else {
			if strings.Contains(strings.ToLower(name), strings.ToLower(filter)) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	return names
}

// UpdateDynamicSelection is deprecated and no-ops. Membership is now determined
// by SubscriptionFilter; selection strategies use health data at Next() time.
func (g *ProxyGroup) UpdateDynamicSelection() {}

// Next returns the currently selected Proxy according to the group type.
// It is the entry point for nested group resolution and creates a fresh
// visited map to detect cycles.
func (g *ProxyGroup) Next() *Proxy {
	return g.NextWithVisited(make(map[string]bool))
}

// NextWithVisited returns the currently selected Proxy while tracking visited
// group names to prevent infinite recursion on cyclic group references.
// If the selected member is a nested group that returns nil, the next member
// is tried according to the group's selection type.
func (g *ProxyGroup) NextWithVisited(visited map[string]bool) *Proxy {
	if g.Name != "" {
		if visited[g.Name] {
			return nil
		}
		visited[g.Name] = true
	}

	g.selMu.RLock()
	members := g.activeMembersLocked()
	g.selMu.RUnlock()

	if len(members) == 0 {
		return nil
	}

	list := make([]GroupMember, len(members))
	copy(list, members)

	for len(list) > 0 {
		m := g.pickMember(list)
		if m.Name == "" {
			return nil
		}
		p := g.resolveMemberWithVisited(m, visited)
		if p != nil {
			return p
		}
		// Inner group returned nil; remove this member and keep looking.
		list = removeGroupMember(list, m)
	}
	return nil
}

// PickActiveMember returns the GroupMember that Next() would select before
// resolving nested groups. It is useful for admin UIs that need to highlight
// the active row. If the selected member is a nested group, the group member
// itself is returned as long as the inner group has a selectable member.
func (g *ProxyGroup) PickActiveMember() GroupMember {
	visited := make(map[string]bool)
	if g.Name != "" {
		if visited[g.Name] {
			return GroupMember{}
		}
		visited[g.Name] = true
	}

	g.selMu.RLock()
	members := g.activeMembersLocked()
	g.selMu.RUnlock()

	if len(members) == 0 {
		return GroupMember{}
	}

	list := make([]GroupMember, len(members))
	copy(list, members)

	for len(list) > 0 {
		m := g.pickMember(list)
		if m.Name == "" {
			return GroupMember{}
		}
		if m.IsGroup {
			if g.resolveGroup != nil {
				if inner := g.resolveGroup(m.Name); inner != nil {
					if inner.PickActiveMember().Name != "" {
						return m
					}
				}
			}
		} else {
			return m
		}
		// Inner group returned no selectable member; keep looking.
		list = removeGroupMember(list, m)
	}
	return GroupMember{}
}

// pickMember applies the group's selection strategy to the supplied member
// list. The list is assumed to be non-empty.
func (g *ProxyGroup) pickMember(list []GroupMember) GroupMember {
	switch g.Type {
	case GroupSelect:
		return g.nextSelectMember(list)
	case GroupLoadBalance:
		return g.nextRoundRobinAliveMember(list)
	case GroupBest:
		return g.nextByHealthMember(list)
	default:
		return list[0]
	}
}

// resolveMemberWithVisited resolves a single member. Nested groups are
// resolved recursively using the shared visited map.
func (g *ProxyGroup) resolveMemberWithVisited(m GroupMember, visited map[string]bool) *Proxy {
	if m.IsGroup {
		if g.resolveGroup != nil {
			if inner := g.resolveGroup(m.Name); inner != nil {
				return inner.NextWithVisited(visited)
			}
		}
		return nil
	}
	return g.ResolveMember(m)
}

func removeGroupMember(list []GroupMember, m GroupMember) []GroupMember {
	for i, x := range list {
		if x == m {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// HealthSnapshot returns a snapshot of the group's health check data.
// Each entry maps a member health key to its current health status.
func (g *ProxyGroup) HealthSnapshot() map[string]HealthInfo {
	g.healthMu.RLock()
	defer g.healthMu.RUnlock()
	result := make(map[string]HealthInfo, len(g.healthMap))
	for key, hs := range g.healthMap {
		result[key] = HealthInfo{
			Alive:     hs.alive,
			Latency:   hs.latency,
			FailCount: hs.failCount,
			LastCheck: hs.lastCheck,
		}
	}
	return result
}

// HealthInfo is a public snapshot of a proxy's health status.
type HealthInfo struct {
	Alive     bool
	Latency   time.Duration
	FailCount int
	LastCheck time.Time
}

// nextSelectMember returns the active member for a select group. If the
// configured ActiveMember is not present in the member list (or is empty), the
// first member is returned. Health status is intentionally ignored so that the
// user's explicit selection is honored, matching the behavior of mainstream
// clients.
func (g *ProxyGroup) nextSelectMember(members []GroupMember) GroupMember {
	if g.ActiveMember != "" {
		for _, m := range members {
			if m.Name == g.ActiveMember {
				return m
			}
		}
	}
	if len(members) > 0 {
		return members[0]
	}
	return GroupMember{}
}

// nextRoundRobinAliveMember returns the next alive member in round-robin from
// the given list, skipping dead ones. If all members have been checked and are
// dead, returns a zero member so the caller can fall through to the next rule
// (e.g. MATCH, DIRECT).
func (g *ProxyGroup) nextRoundRobinAliveMember(members []GroupMember) GroupMember {
	count := len(members)
	if count == 0 {
		return GroupMember{}
	}
	g.healthMu.RLock()
	defer g.healthMu.RUnlock()

	// Try up to count times to find an alive one
	for i := 0; i < count; i++ {
		idx := atomic.AddInt64(&g.idx, 1) - 1
		m := members[idx%int64(count)]
		key := m.HealthKey()
		if h, ok := g.healthMap[key]; ok {
			if h.alive {
				return m
			}
		} else {
			// no health data yet, assume healthy and return
			return m
		}
	}
	// all members have health data and all are dead
	return GroupMember{}
}

// nextByHealthMember returns the alive member with the lowest latency from the
// given list. If all members have been checked and are dead, returns a zero
// member so the caller can fall through to the next rule (e.g. MATCH, DIRECT).
func (g *ProxyGroup) nextByHealthMember(members []GroupMember) GroupMember {
	g.healthMu.RLock()
	defer g.healthMu.RUnlock()

	var best GroupMember
	var bestLatency time.Duration
	allChecked := true
	for _, m := range members {
		key := m.HealthKey()
		h, ok := g.healthMap[key]
		if ok {
			if !h.alive {
				continue
			}
			if best.Name == "" || h.latency < bestLatency {
				best = m
				bestLatency = h.latency
			}
		} else {
			// no health data yet for this member
			allChecked = false
		}
	}
	if best.Name != "" {
		return best
	}
	// fallback: if not all members have been checked, assume the unchecked ones are healthy and return the first one
	if !allChecked {
		for _, m := range members {
			if _, ok := g.healthMap[m.HealthKey()]; !ok {
				return m
			}
		}
	}
	return GroupMember{}
}

// SetHealth records the result of a single health check. It uses consecutive
// sampling: a member is marked dead only after healthFailThreshold consecutive
// failures, and marked alive only after healthSuccessThreshold consecutive
// successes. The key must be GroupMember.HealthKey(). It returns true if the
// member's alive state actually changed.
func (g *ProxyGroup) SetHealth(key string, checkAlive bool, latency time.Duration) bool {
	g.healthMu.Lock()
	defer g.healthMu.Unlock()

	h := g.healthMap[key]
	isNew := false
	if h == nil {
		h = &healthStatus{}
		g.healthMap[key] = h
		isNew = true
	}

	h.latency = latency
	h.lastCheck = time.Now()

	if checkAlive {
		h.successCount++
		h.failCount = 0
		if isNew || (!h.alive && h.successCount >= healthSuccessThreshold) {
			h.alive = true
			return true
		}
	} else {
		h.failCount++
		h.successCount = 0
		if isNew || (h.alive && h.failCount >= healthFailThreshold) {
			h.alive = false
			return true
		}
	}
	return false
}

// SetHealthImmediate sets the alive state directly, bypassing the consecutive-
// success/failure thresholds used by periodic automated checks. This is intended
// for explicit manual health tests where the user expects the result to be
// reflected immediately in dynamic subscription selection.
func (g *ProxyGroup) SetHealthImmediate(key string, alive bool, latency time.Duration) bool {
	g.healthMu.Lock()
	defer g.healthMu.Unlock()

	h := g.healthMap[key]
	isNew := false
	if h == nil {
		h = &healthStatus{}
		g.healthMap[key] = h
		isNew = true
	}

	changed := isNew || h.alive != alive
	h.latency = latency
	h.lastCheck = time.Now()
	h.alive = alive
	if alive {
		h.successCount++
		h.failCount = 0
	} else {
		h.failCount++
		h.successCount = 0
	}
	return changed
}

func (g *ProxyGroup) GetHealth(key string) *healthStatus {
	g.healthMu.RLock()
	defer g.healthMu.RUnlock()
	return g.healthMap[key]
}

// GetHealthInfo returns a public snapshot of a member's health status.
func (g *ProxyGroup) GetHealthInfo(key string) HealthInfo {
	g.healthMu.RLock()
	defer g.healthMu.RUnlock()
	if hs, ok := g.healthMap[key]; ok {
		return HealthInfo{
			Alive:     hs.alive,
			Latency:   hs.latency,
			FailCount: hs.failCount,
			LastCheck: hs.lastCheck,
		}
	}
	return HealthInfo{}
}

func (h *healthStatus) IsAlive() bool {
	return h != nil && h.alive
}

// RemoveHealth deletes health status for the given member keys.
func (g *ProxyGroup) RemoveHealth(keys []string) {
	g.healthMu.Lock()
	defer g.healthMu.Unlock()
	for _, key := range keys {
		delete(g.healthMap, key)
	}
}

// CopyHealthFrom copies health data from another ProxyGroup.
func (g *ProxyGroup) CopyHealthFrom(other *ProxyGroup) {
	if other == nil {
		return
	}
	other.healthMu.RLock()
	defer other.healthMu.RUnlock()
	
	g.healthMu.Lock()
	defer g.healthMu.Unlock()
	
	if g.healthMap == nil {
		g.healthMap = make(map[string]*healthStatus)
	}
	for k, v := range other.healthMap {
		g.healthMap[k] = v
	}
}

// ========== Resolver ==========

type Resolver struct {
	Name    string `yaml:"name"`
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	SrcHost string `yaml:"src-host"`
	SrcPort int    `yaml:"src-port"`
	DstHost string `yaml:"dst-host"`
	DstPort int    `yaml:"dst-port"`
}

// IsEnabled reports whether the resolver is enabled. Omitted or nil means enabled.
func (r *Resolver) IsEnabled() bool {
	if r == nil || r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

// ========== RateLimiter ==========

type RateLimiter struct {
	Name           string
	bytesPerSecond int64
	mu             sync.Mutex
	nextSendTimeNs int64
}

func NewRateLimiter(name string, bytesPerSecond int64) *RateLimiter {
	return &RateLimiter{
		Name:           name,
		bytesPerSecond: bytesPerSecond,
		nextSendTimeNs: time.Now().UnixNano(),
	}
}

func (r *RateLimiter) Schedule(bytes int) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.bytesPerSecond <= 0 {
		return 0
	}

	now := time.Now().UnixNano()
	sendTime := now
	if r.nextSendTimeNs > now {
		sendTime = r.nextSendTimeNs
	}
	duration := int64(bytes) * 1_000_000_000 / r.bytesPerSecond
	r.nextSendTimeNs = sendTime + duration

	delay := sendTime - now
	if delay < 0 {
		delay = 0
	}
	return time.Duration(delay)
}

// ========== Matcher (Mather in Java) ==========

// AddrRequest represents a destination address request
type AddrRequest struct {
	DstAddr string
	DstPort int
	// CmdType: "CONNECT" or "BIND"
	CmdType string
}

func NewConnectRequest(dstAddr string, dstPort int) *AddrRequest {
	return &AddrRequest{DstAddr: dstAddr, DstPort: dstPort, CmdType: "CONNECT"}
}

func NewBindRequest(dstAddr string, dstPort int) *AddrRequest {
	return &AddrRequest{DstAddr: dstAddr, DstPort: dstPort, CmdType: "BIND"}
}

type Matcher interface {
	Match(request *AddrRequest, mapping *Mapping) string
}

// parseProxyName splits "PROXY#MAPPING" into (proxy, mapping).
// If no "#" is present, mapping is empty.
func parseProxyName(proxyName string) (name, mapping string) {
	parts := strings.SplitN(proxyName, "#", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return proxyName, ""
}

// DomainSuffixMatcher
type DomainSuffixMatcher struct {
	proxyName    string
	domainSuffix string
	mappingName  string
}

func NewDomainSuffixMatcher(proxyName, domainSuffix string) *DomainSuffixMatcher {
	name, mn := parseProxyName(proxyName)
	return &DomainSuffixMatcher{
		proxyName:    name,
		domainSuffix: domainSuffix,
		mappingName:  mn,
	}
}

func (m *DomainSuffixMatcher) Match(req *AddrRequest, mapping *Mapping) string {
	if strings.HasSuffix(req.DstAddr, m.domainSuffix) {
		if m.mappingName == "" || (mapping != nil && m.mappingName == mapping.Name) {
			return m.proxyName
		}
	}
	return ""
}

// IpCidrMatcher
type IpCidrMatcher struct {
	proxyName   string
	network     uint32
	mask        uint32
	mappingName string
}

func NewIpCidrMatcher(proxyName, cidr string) *IpCidrMatcher {
	name, mn := parseProxyName(proxyName)

	cidrParts := strings.SplitN(cidr, "/", 2)
	if len(cidrParts) != 2 {
		// Invalid CIDR: match nothing
		return &IpCidrMatcher{proxyName: name, network: 0, mask: 0xffffffff, mappingName: mn}
	}
	ipStr := cidrParts[0]
	prefixLen, err := strconv.Atoi(cidrParts[1])
	if err != nil || prefixLen < 0 || prefixLen > 32 {
		return &IpCidrMatcher{proxyName: name, network: 0, mask: 0xffffffff, mappingName: mn}
	}

	mask := uint32(0xffffffff) << (32 - prefixLen)
	network := ip2Uint32(ipStr)
	if network == 0 && ipStr != "0.0.0.0" {
		// Invalid IP: match nothing
		return &IpCidrMatcher{proxyName: name, network: 0, mask: 0xffffffff, mappingName: mn}
	}

	return &IpCidrMatcher{
		proxyName:   name,
		network:     network,
		mask:        mask,
		mappingName: mn,
	}
}

func (m *IpCidrMatcher) Match(req *AddrRequest, mapping *Mapping) string {
	ip := net.ParseIP(req.DstAddr)
	if ip == nil {
		return ""
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return ""
	}
	ipInt := binary.BigEndian.Uint32(ip4)
	if (ipInt & m.mask) == (m.network & m.mask) {
		if m.mappingName == "" || (mapping != nil && m.mappingName == mapping.Name) {
			return m.proxyName
		}
	}
	return ""
}

func ip2Uint32(ipStr string) uint32 {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return 0
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return binary.BigEndian.Uint32(ip4)
}

// MatchAllMatcher (MATCH rule)
type MatchAllMatcher struct {
	proxyName   string
	mappingName string
}

func NewMatchAllMatcher(proxyName string) *MatchAllMatcher {
	name, mn := parseProxyName(proxyName)
	return &MatchAllMatcher{
		proxyName:   name,
		mappingName: mn,
	}
}

func (m *MatchAllMatcher) Match(req *AddrRequest, mapping *Mapping) string {
	if m.mappingName == "" || (mapping != nil && m.mappingName == mapping.Name) {
		return m.proxyName
	}
	return ""
}

func (m *MatchAllMatcher) ProxyName() string   { return m.proxyName }
func (m *MatchAllMatcher) MappingName() string { return m.mappingName }

// ========== RuleConfiguration ==========

// Default UDP ephemeral port range (IANA dynamic ports: 49152-65535)
const (
	DefaultUDPPortMin = 49152
	DefaultUDPPortMax = 65535
)

type RuleConfiguration struct {
	Proxies       []*Proxy        `yaml:"proxies"`
	ProxyGroups   []*ProxyGroup   `yaml:"proxy-groups"`
	Subscriptions []*Subscription `yaml:"subscriptions"`
	Rules         []string        `yaml:"rules"`
	Mappings      []*Mapping      `yaml:"mappings"`
	Resolvers     []*Resolver     `yaml:"resolvers"`

	// ReverseConfigs holds multiple reverse client configurations within a
	// single process.
	ReverseConfigs []*ReverseConfig `yaml:"reverse-configs,omitempty"`

	// Enable interactive console setup when no config file is found.
	// Default: true in default.yaml, false in user configs.
	Interactive bool `yaml:"interactive"`

	// Global UDP port range for all ephemeral UDP bindings.
	// Format: "min-max" e.g. "30000-30100". Default: OS-assigned.
	UDPPortRange string `yaml:"udp-port-range"`

	// TCP port range for reverse-side listener allocation (registry side).
	// Format: "min-max" e.g. "20000-20100". Empty = no restriction.
	TCPPortRange string `yaml:"tcp-port-range"`

	// Web admin panel configuration.
	Admin *AdminConfig `yaml:"admin,omitempty"`

	// TUN traffic interception configuration.
	TUN *TUNConfig `yaml:"tun,omitempty"`

	// Initialized fields
	ProxyNames        map[string]*Proxy        `yaml:"-"`
	GroupNames        map[string]*ProxyGroup   `yaml:"-"`
	SubscriptionNames map[string]*Subscription `yaml:"-"`
	Matchers          []Matcher                `yaml:"-"`
	mu                sync.RWMutex             `yaml:"-"`

	// Parsed UDP port range
	udpPortMin int // 0 = use OS default
	udpPortMax int
}

// AdminConfig holds web admin settings.
type AdminConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Addr        string `yaml:"addr"`
	Token       string `yaml:"token"`
	AuthEnabled bool   `yaml:"auth-enabled"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	TLSCert     string `yaml:"tls-cert,omitempty"`
	TLSKey      string `yaml:"tls-key,omitempty"`
}

// ReverseConfig holds configuration for running as a reverse client.
type ReverseConfig struct {
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	Name              string `yaml:"name,omitempty" json:"name,omitempty"`
	Seq               int    `yaml:"seq,omitempty" json:"seq,omitempty"`
	RegistryProxy     string `yaml:"registry-proxy" json:"registry-proxy"`
	PreferredPort     int    `yaml:"preferred-port" json:"preferred-port"`
	TargetAddress     string `yaml:"target-address" json:"target-address"`
	ReconnectInterval int    `yaml:"reconnect-interval" json:"reconnect-interval"`
	RegistryUser      string `yaml:"registry-user,omitempty" json:"registry-user,omitempty"`
	RegistryPassword  string `yaml:"registry-password,omitempty" json:"registry-password,omitempty"`
	RegistrySNI       string `yaml:"registry-sni,omitempty" json:"registry-sni,omitempty"`
	ListenerProto     string `yaml:"listener-proto,omitempty" json:"listener-proto,omitempty"`
	ListenerUser      string `yaml:"listener-user,omitempty" json:"listener-user,omitempty"`
	ListenerPassword  string `yaml:"listener-password,omitempty" json:"listener-password,omitempty"`
	ListenerSNI       string `yaml:"listener-sni,omitempty" json:"listener-sni,omitempty"`
	DirectDstHost     string `yaml:"direct-dst-host,omitempty" json:"direct-dst-host,omitempty"`
	DirectDstPort     int    `yaml:"direct-dst-port,omitempty" json:"direct-dst-port,omitempty"`
	SkipCertVerify    bool   `yaml:"skip-cert-verify,omitempty" json:"skip-cert-verify,omitempty"`
	// LastError holds the most recent reverse client registration error so the
	// admin UI can surface it instead of showing "waiting for port" forever.
	LastError string `yaml:"-" json:"last-error,omitempty"`
	// AssignedPort is the actual port allocated by the registry after a
	// successful reverse registration. It is informational only and is updated
	// by the reverse client at runtime.
	AssignedPort int `yaml:"-" json:"assigned-port,omitempty"`
}

// TUNConfig holds TUN traffic interception settings.
type TUNConfig struct {
	Enabled          *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ProbeURLs        []string `yaml:"probe-urls,omitempty" json:"probe-urls,omitempty"`
	DirectNameserver []string `yaml:"direct-nameserver,omitempty" json:"direct-nameserver,omitempty"`
}

// IsEnabled reports whether TUN is enabled. Omitted or nil means enabled
// (auto-enable when the TUN extension is available).
func (t *TUNConfig) IsEnabled() bool {
	if t == nil || t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// ProbeURLList returns the configured TUN watchdog probe URLs, or nil if none
// are configured. Callers should fall back to tun.DefaultProbeURLs when nil/empty.
func (t *TUNConfig) ProbeURLList() []string {
	if t == nil {
		return nil
	}
	return t.ProbeURLs
}

// DirectNameserverList returns the configured DNS servers for DIRECT connection
// resolution, or nil if none are configured.
func (t *TUNConfig) DirectNameserverList() []string {
	if t == nil {
		return nil
	}
	return t.DirectNameserver
}

func LoadRaw(filePath string) (*RuleConfiguration, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read config file fail: %w", err)
	}
	return LoadRawBytes(data)
}

func LoadRawBytes(data []byte) (*RuleConfiguration, error) {
	data = SubstituteEnv(data)
	var conf RuleConfiguration
	if err := yaml.Unmarshal(data, &conf); err != nil {
		return nil, fmt.Errorf("parse config fail: %w", err)
	}
	return &conf, nil
}

// SaveRaw writes the RuleConfiguration back to a YAML file atomically.
// It writes to a temporary file and renames it to avoid partial reads
// by fsnotify watchers or other processes.
// Comments and custom formatting are not preserved.
func SaveRaw(filePath string, conf *RuleConfiguration) error {
	data, err := yaml.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal config fail: %w", err)
	}

	// Atomic write: write to temp file, then rename.
	// This ensures watchers never see a partially-written file.
	tmpPath := filePath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("write temp config file fail: %w", err)
	}
	if err := os.Rename(tmpPath, filePath); err != nil {
		os.Remove(tmpPath) // best-effort cleanup
		return fmt.Errorf("rename config file fail: %w", err)
	}
	return nil
}

func Load(filePath string) (*RuleConfiguration, error) {
	conf, err := LoadRaw(filePath)
	if err != nil {
		return nil, err
	}
	if err := conf.Init(); err != nil {
		return nil, err
	}
	return conf, nil
}

// Merge merges override config into this (base) config.
// Same-name items are replaced, different-name items are appended.
// Rules from override are prepended (env rules take priority).
func (c *RuleConfiguration) Merge(override *RuleConfiguration) error {
	if override == nil {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// proxies
	if len(override.Proxies) > 0 {
		if c.Proxies == nil {
			c.Proxies = make([]*Proxy, 0)
		}
		mergeByName(
			&c.Proxies, override.Proxies,
			func(p *Proxy) string { return p.Name },
		)
	}

	// subscriptions
	if len(override.Subscriptions) > 0 {
		if c.Subscriptions == nil {
			c.Subscriptions = make([]*Subscription, 0)
		}
		mergeByName(
			&c.Subscriptions, override.Subscriptions,
			func(s *Subscription) string { return s.Name },
		)
	}

	// proxyGroups
	if len(override.ProxyGroups) > 0 {
		if c.ProxyGroups == nil {
			c.ProxyGroups = make([]*ProxyGroup, 0)
		}
		mergeByName(
			&c.ProxyGroups, override.ProxyGroups,
			func(p *ProxyGroup) string { return p.Name },
		)
	}

	// mappings
	if len(override.Mappings) > 0 {
		if c.Mappings == nil {
			c.Mappings = make([]*Mapping, 0)
		}
		mergeByName(
			&c.Mappings, override.Mappings,
			func(p *Mapping) string { return p.Name },
		)
	}

	// resolvers
	if len(override.Resolvers) > 0 {
		if c.Resolvers == nil {
			c.Resolvers = make([]*Resolver, 0)
		}
		mergeByName(
			&c.Resolvers, override.Resolvers,
			func(p *Resolver) string { return p.Name },
		)
	}

	// rules: env rules prepend (higher priority)
	if len(override.Rules) > 0 {
		if c.Rules == nil {
			c.Rules = make([]string, 0, len(override.Rules))
		}
		merged := make([]string, 0, len(override.Rules)+len(c.Rules))
		merged = append(merged, override.Rules...)
		merged = append(merged, c.Rules...)
		c.Rules = merged
	}

	// Rebuild internal mappings so Match/Resolving use up-to-date data.
	return c.Init()
}

func mergeByName[T any](baseList *[]T, overrideList []T, nameExtractor func(T) string) {
	nameToIdx := make(map[string]int, len(*baseList))
	for i, item := range *baseList {
		nameToIdx[nameExtractor(item)] = i
	}
	for _, item := range overrideList {
		name := nameExtractor(item)
		if idx, ok := nameToIdx[name]; ok {
			(*baseList)[idx] = item
		} else {
			*baseList = append(*baseList, item)
			nameToIdx[name] = len(*baseList) - 1
		}
	}
}

func (c *RuleConfiguration) setNext(proxy *Proxy, visited map[*Proxy]bool) error {
	next, ok := c.ProxyNames[proxy.ViaProxy]
	if !ok || next == nil {
		// If the name matches a proxy group, resolve it directly.
		// The group returns a *Proxy from its internal subscription pool,
		// nested groups, or the global proxy namespace.
		if group, gok := c.GroupNames[proxy.ViaProxy]; gok && group != nil {
			next = group.NextWithVisited(make(map[string]bool))
			if next != nil {
				ok = true
			}
		}
	}
	if !ok || next == nil {
		proxy.Next = SingletonProxy(ProxyDIRECT)
		return nil
	}
	if visited[next] {
		return fmt.Errorf("recycle proxy: %s", proxy.ViaProxy)
	}
	visited[next] = true
	proxy.Next = next
	return c.setNext(next, visited)
}

func (c *RuleConfiguration) Init() error {
	c.ProxyNames = make(map[string]*Proxy)
	c.GroupNames = make(map[string]*ProxyGroup)
	c.SubscriptionNames = make(map[string]*Subscription)
	c.ProxyNames[ProxyDIRECT] = SingletonProxy(ProxyDIRECT)
	c.ProxyNames[ProxyREJECT] = SingletonProxy(ProxyREJECT)

	// Register proxies
	for _, proxy := range c.Proxies {
		if !proxy.IsEnabled() {
			continue
		}
		proxy.Type = strings.ToLower(proxy.Type)
		if _, exists := c.ProxyNames[proxy.Name]; exists {
			return fmt.Errorf("proxyName conflict: %s", proxy.Name)
		}
		if _, exists := c.GroupNames[proxy.Name]; exists {
			return fmt.Errorf("proxyName conflict with group: %s", proxy.Name)
		}
		if _, exists := c.SubscriptionNames[proxy.Name]; exists {
			return fmt.Errorf("proxyName conflict with subscription: %s", proxy.Name)
		}
		c.ProxyNames[proxy.Name] = proxy

		// Convert Clash/Mihomo shorthand bandwidth (Mbps) to bytes per second.
		if proxy.Up > 0 && proxy.UpBps == 0 {
			proxy.UpBps = proxy.Up * 125000
		}
		if proxy.Down > 0 && proxy.DownBps == 0 {
			proxy.DownBps = proxy.Down * 125000
		}

		if proxy.UpBps > 0 {
			proxy.UpRateLimiter = NewRateLimiter("UpRateLimiter_"+proxy.Name, proxy.UpBps)
		}
		if proxy.DownBps > 0 {
			proxy.DownRateLimiter = NewRateLimiter("DownRateLimiter_"+proxy.Name, proxy.DownBps)
		}
	}

	// Register subscriptions before groups so groups can reference them.
	for _, sub := range c.Subscriptions {
		if !sub.IsEnabled() {
			continue
		}
		if _, exists := c.ProxyNames[sub.Name]; exists {
			return fmt.Errorf("subscription name conflict with proxy: %s", sub.Name)
		}
		if _, exists := c.GroupNames[sub.Name]; exists {
			return fmt.Errorf("subscription name conflict with group: %s", sub.Name)
		}
		if _, exists := c.SubscriptionNames[sub.Name]; exists {
			return fmt.Errorf("subscription name conflict: %s", sub.Name)
		}
		if sub.SubProxies == nil {
			sub.SubProxies = make(map[string]*Proxy)
		}
		c.SubscriptionNames[sub.Name] = sub
	}

	// Register proxy groups FIRST so setNext can resolve proxy chains
	// that reference group names (e.g. proxy: MGMS_NET).
	for _, group := range c.ProxyGroups {
		if !group.IsEnabled() {
			continue
		}
		if err := group.Init(); err != nil {
			return err
		}
		if _, exists := c.ProxyNames[group.Name]; exists {
			return fmt.Errorf("groupName conflict with proxy: %s", group.Name)
		}
		if _, exists := c.GroupNames[group.Name]; exists {
			return fmt.Errorf("groupName conflict: %s", group.Name)
		}
		if _, exists := c.SubscriptionNames[group.Name]; exists {
			return fmt.Errorf("groupName conflict with subscription: %s", group.Name)
		}
		c.GroupNames[group.Name] = group
	}

	// Detect cyclic group references before callbacks are injected.
	for _, group := range c.ProxyGroups {
		if !group.IsEnabled() {
			continue
		}
		if err := c.checkGroupCycle(group.Name, make(map[string]bool)); err != nil {
			return err
		}
	}

	// Inject lookup callbacks into each group.
	for _, group := range c.ProxyGroups {
		g := group
		g.resolveProxy = func(name string) *Proxy {
			return c.ProxyNames[name]
		}
		g.resolveGroup = func(name string) *ProxyGroup {
			return c.GroupNames[name]
		}
		g.resolveSubscription = func(name string) *Subscription {
			return c.SubscriptionNames[name]
		}
	}

	// Rebuild runtime member lists for all groups now that lookup callbacks are
	// in place. This is required for manual-only groups to work immediately.
	for _, group := range c.ProxyGroups {
		group.RebuildProxies()
	}

	// Validate group subscription references.
	for _, group := range c.ProxyGroups {
		if !group.IsEnabled() {
			continue
		}
		if group.Subscription != "" {
			if _, ok := c.SubscriptionNames[group.Subscription]; !ok {
				return fmt.Errorf("group %s references unknown subscription: %s", group.Name, group.Subscription)
			}
		}
	}

	// Set proxy chains (must run after groups are registered)
	for _, proxy := range c.Proxies {
		visited := map[*Proxy]bool{}
		if err := c.setNext(proxy, visited); err != nil {
			return err
		}
	}

	// Build matchers
	c.Matchers = make([]Matcher, 0, len(c.Rules))
	for _, rule := range c.Rules {
		trimmed := strings.TrimSpace(rule)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		rs := strings.SplitN(rule, ",", 3)
		if len(rs) < 2 {
			continue
		}
		switch rs[0] {
		case "DOMAIN-SUFFIX":
			if len(rs) < 3 {
				continue
			}
			c.Matchers = append(c.Matchers, NewDomainSuffixMatcher(strings.Trim(rs[2], `"'`), strings.Trim(rs[1], `"'`)))
		case "IP-CIDR":
			if len(rs) < 3 {
				continue
			}
			c.Matchers = append(c.Matchers, NewIpCidrMatcher(strings.Trim(rs[2], `"'`), strings.Trim(rs[1], `"'`)))
		case "MATCH":
			c.Matchers = append(c.Matchers, NewMatchAllMatcher(strings.Trim(rs[1], `"'`)))
		default:
			return fmt.Errorf("unsupported matcher type: %s", rs[0])
		}
	}

	// Parse UDP port range
	if c.UDPPortRange != "" {
		parts := strings.SplitN(c.UDPPortRange, "-", 2)
		if len(parts) == 2 {
			min, err1 := strconv.Atoi(parts[0])
			max, err2 := strconv.Atoi(parts[1])
			if err1 == nil && err2 == nil && min > 0 && max <= 65535 && min <= max {
				c.udpPortMin = min
				c.udpPortMax = max
			} else {
				return fmt.Errorf("invalid udp-port-range: %s (expected format: min-max, e.g. 30000-30100)", c.UDPPortRange)
			}
		} else {
			return fmt.Errorf("invalid udp-port-range: %s (expected format: min-max)", c.UDPPortRange)
		}
	}

	return nil
}

func (c *RuleConfiguration) Lock() {
	c.mu.Lock()
}

func (c *RuleConfiguration) Unlock() {
	c.mu.Unlock()
}

func (c *RuleConfiguration) RLock() {
	c.mu.RLock()
}

func (c *RuleConfiguration) RUnlock() {
	c.mu.RUnlock()
}

// HasReverseAddress returns true if any mapping uses the given address for
// reverse connections. Used by the passive side to reject reverse registrations
// for addresses that are no longer configured.
func (c *RuleConfiguration) HasReverseAddress(addr string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, p := range c.Proxies {
		if !p.IsEnabled() {
			continue
		}
		if p.ReverseAddress == addr {
			return true
		}
	}
	return false
}

// Match finds the matching proxy for the given request and mapping.
// It resolves Group names by delegating to group.Next(), which returns a *Proxy
// from the group's internal subscription pool or the global namespace.
func (c *RuleConfiguration) Match(request *AddrRequest, mapping *Mapping) *Proxy {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, matcher := range c.Matchers {
		proxyName := matcher.Match(request, mapping)
		if proxyName == "" {
			continue
		}
		return c.resolveName(proxyName)
	}
	return nil
}

func (c *RuleConfiguration) resolveName(name string) *Proxy {
	if proxy, ok := c.ProxyNames[name]; ok && proxy != nil {
		return proxy
	}
	group, ok := c.GroupNames[name]
	if !ok || len(group.Members) == 0 {
		return nil
	}
	return group.NextWithVisited(make(map[string]bool))
}

// checkGroupCycle detects cyclic references among proxy groups through manual
// members. It is called during Init() so invalid configs fail early.
func (c *RuleConfiguration) checkGroupCycle(name string, visited map[string]bool) error {
	if visited[name] {
		return fmt.Errorf("proxy group cycle detected involving %s", name)
	}
	visited[name] = true
	defer delete(visited, name)

	group, ok := c.GroupNames[name]
	if !ok {
		return nil
	}
	for _, m := range group.ManualProxies {
		if _, isGroup := c.GroupNames[m]; isGroup {
			if err := c.checkGroupCycle(m, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

// Resolving applies resolver rules to transform the request address
func (c *RuleConfiguration) Resolving(req *AddrRequest) *AddrRequest {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.Resolvers == nil {
		return req
	}
	for _, resolver := range c.Resolvers {
		if !resolver.IsEnabled() {
			continue
		}
		if req.DstAddr == resolver.SrcHost && req.DstPort == resolver.SrcPort {
			return &AddrRequest{
				DstAddr: resolver.DstHost,
				DstPort: resolver.DstPort,
				CmdType: req.CmdType,
			}
		}
	}
	return req
}

// NeedHysteria2 checks if any proxy uses hysteria2
func (c *RuleConfiguration) NeedHysteria2() bool {
	for _, proxy := range c.Proxies {
		if !proxy.IsEnabled() {
			continue
		}
		if proxy.Type == ProxyHYSTERIA2 {
			return true
		}
	}
	return false
}

// UDPPortRangeValues returns the configured UDP ephemeral port range.
// If not configured (0, 0), the caller should use OS default (net.ListenUDP with nil addr).
func (c *RuleConfiguration) UDPPortRangeValues() (min, max int) {
	return c.udpPortMin, c.udpPortMax
}
