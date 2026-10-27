package db

import (
	"fmt"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"phaethon/config"
	"phaethon/util"
)

// RuleMatcher 是统一的规则匹配器
// 所有配置（静态+动态）存储在对象 Map 中，通过 Watch 实时同步
type RuleMatcher struct {
	// 统一对象 Map（包含静态+动态）
	proxies       map[string]*config.Proxy
	groups        map[string]*config.ProxyGroup
	subscriptions map[string]*config.Subscription
	mappings      map[string]*config.Mapping
	rules         []*config.Rule
	resolvers     []*config.Resolver
	mesh          *config.MeshConfig

	// 编译后的匹配器（从 rules 生成）
	matchers          []config.Matcher
	matcherTimeRanges []config.TimeRange

	// 保护并发访问
	mu sync.RWMutex
}

// BuildRuleMatcher 从数据库构建 RuleMatcher
func BuildRuleMatcher() (*RuleMatcher, error) {
	m := &RuleMatcher{
		proxies:       make(map[string]*config.Proxy),
		groups:        make(map[string]*config.ProxyGroup),
		subscriptions: make(map[string]*config.Subscription),
		mappings:      make(map[string]*config.Mapping),
	}

	// 从数据库加载规则匹配需要的数据
	err := View(func(tx *bolt.Tx) error {
		// 加载代理到 map
		proxies, err := listTx[config.Proxy](tx, BucketProxies)
		if err != nil {
			return fmt.Errorf("load proxies: %w", err)
		}
		for _, p := range proxies {
			m.proxies[p.Name] = p
		}

		// 加载代理组到 map
		groups, err := listTx[config.ProxyGroup](tx, BucketProxyGroups)
		if err != nil {
			return fmt.Errorf("load proxy groups: %w", err)
		}
		for _, g := range groups {
			m.groups[g.Name] = g
		}

		// 加载订阅到 map（元数据 + 节点池）
		subs, err := listTx[config.Subscription](tx, BucketSubscriptions)
		if err != nil {
			return fmt.Errorf("load subscriptions: %w", err)
		}
		for _, sub := range subs {
			// 直接在当前事务中读取订阅节点，避免嵌套事务
			var nodesData *SubscriptionNodes
			nodesData, err = getTx[SubscriptionNodes](tx, BucketSubscriptionNodes, []byte(sub.Name))
			if err != nil && err != ErrNotFound {
				return fmt.Errorf("load subscription nodes for %s: %w", sub.Name, err)
			}
			var nodes map[string]*config.Proxy
			if nodesData != nil {
				nodes = nodesData.Proxies
			}
			if nodes != nil {
				sub.SubMu.Lock()
				sub.SubProxies = nodes
				sub.SubMu.Unlock()
			}
			m.subscriptions[sub.Name] = sub
		}

		// 加载规则（字符串，后续解析为 Rule 对象）
		ruleStrs, err := listRulesTx(tx)
		if err != nil {
			return fmt.Errorf("load rules: %w", err)
		}
		m.rules = parseRules(ruleStrs)

		// 加载映射到 map
		mappings, err := listTx[config.Mapping](tx, BucketMappings)
		if err != nil {
			return fmt.Errorf("load mappings: %w", err)
		}
		for _, mp := range mappings {
			m.mappings[mp.Name] = mp
		}

		// 加载解析器
		if m.resolvers, err = listTx[config.Resolver](tx, BucketResolvers); err != nil {
			return fmt.Errorf("load resolvers: %w", err)
		}

		// 加载 Mesh 配置（仅用于 resolver 的 src-relative）
		cb := tx.Bucket(BucketMeshConfig)
		if cb != nil {
			var mesh config.MeshConfig
			if present, err := readScalarInto(cb, keyMesh, &mesh); err != nil {
				return err
			} else if present {
				m.mesh = &mesh
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// 调用 init() 构建对象关系
	if err := m.init(); err != nil {
		return nil, fmt.Errorf("config init: %w", err)
	}

	return m, nil
}

// parseRules 将规则字符串解析为 Rule 对象
func parseRules(ruleStrs []string) []*config.Rule {
	rules := make([]*config.Rule, 0, len(ruleStrs))
	for _, ruleStr := range ruleStrs {
		rule := parseRule(ruleStr)
		if rule != nil {
			rules = append(rules, rule)
		}
	}
	return rules
}

// parseRule 解析单条规则字符串
func parseRule(ruleStr string) *config.Rule {
	parts := strings.Split(ruleStr, ",")
	if len(parts) < 2 {
		return nil
	}
	
	ruleType := strings.TrimSpace(parts[0])
	proxyName, mappingName, tr := config.ParseProxyName(strings.TrimSpace(parts[len(parts)-1]))
	
	var pattern string
	if len(parts) >= 3 {
		pattern = strings.TrimSpace(parts[1])
	}
	
	return &config.Rule{
		Type:        ruleType,
		Pattern:     pattern,
		ProxyName:   proxyName,
		MappingName: mappingName,
		TimeRange:   &tr,
	}
}

// ResolveMatch 执行规则匹配
func (m *RuleMatcher) ResolveMatch(req *config.AddrRequest, mappingName string) (
	*config.AddrRequest, *config.Proxy, *config.MatchResult, error) {

	util.LogModuleInfo("system", "[RESOLVE-DEBUG] ResolveMatch called: dst=%s:%d mapping=%s", req.DstAddr, req.DstPort, mappingName)

	// Apply resolver rules first
	req = m.resolving(req)

	var mapping *config.Mapping
	if mappingName != "" {
		mapping = m.findMapping(mappingName)
		util.LogModuleInfo("system", "[RESOLVE-DEBUG] findMapping(%s) = %v", mappingName, mapping != nil)
	}

	proxy, matchResult := m.match(req, mapping)
	util.LogModuleInfo("system", "[RESOLVE-DEBUG] ResolveMatch returning: proxy=%v matchResult=%v", proxy != nil, matchResult != nil)
	return req, proxy, matchResult, nil
}

// init builds internal lookup maps and matchers from loaded data
func (m *RuleMatcher) init() error {
	// 添加内置代理
	m.proxies[config.ProxyDIRECT] = config.SingletonProxy(config.ProxyDIRECT)
	m.proxies[config.ProxyREJECT] = config.SingletonProxy(config.ProxyREJECT)

	// 添加内置映射（TUN）
	m.mappings["TUN"] = &config.Mapping{Name: "TUN", Type: "tun"}

	// 初始化代理（类型转换、速率限制器等）
	for _, proxy := range m.proxies {
		if !proxy.IsEnabled() {
			continue
		}
		proxy.Type = strings.ToLower(proxy.Type)

		// Convert Clash/Mihomo shorthand bandwidth (Mbps) to bytes per second.
		if proxy.Up > 0 && proxy.UpBps == 0 {
			proxy.UpBps = proxy.Up * 125000
		}
		if proxy.Down > 0 && proxy.DownBps == 0 {
			proxy.DownBps = proxy.Down * 125000
		}

		if proxy.UpBps > 0 {
			proxy.UpRateLimiter = config.NewRateLimiter("UpRateLimiter_"+proxy.Name, proxy.UpBps)
		}
		if proxy.DownBps > 0 {
			proxy.DownRateLimiter = config.NewRateLimiter("DownRateLimiter_"+proxy.Name, proxy.DownBps)
		}
	}

	// 初始化订阅
	for _, sub := range m.subscriptions {
		if !sub.IsEnabled() {
			continue
		}
		if sub.SubProxies == nil {
			sub.SubProxies = make(map[string]*config.Proxy)
		}
	}

	// Validate and precompute resolvers.
	nodeID := ""
	if m.mesh != nil {
		nodeID = strings.TrimSpace(m.mesh.NodeID)
	}
	for _, resolver := range m.resolvers {
		if resolver == nil {
			continue
		}
		resolver.Proto = strings.ToLower(strings.TrimSpace(resolver.Proto))
		switch resolver.Proto {
		case "", "tcp", "udp":
		default:
			return fmt.Errorf("resolver %s: invalid proto %q (must be tcp or udp)", resolver.Name, resolver.Proto)
		}
		resolver.SrcFull = ""
		if resolver.SrcRelative {
			if strings.TrimSpace(resolver.SrcHost) == "" {
				return fmt.Errorf("resolver %s: src-relative requires src-host", resolver.Name)
			}
			if nodeID == "" {
				return fmt.Errorf("resolver %s: src-relative requires mesh node-id", resolver.Name)
			}
			resolver.SrcFull = resolver.SrcHost + "." + nodeID + ".phn"
		}
	}

	// Inject built-in loopback resolver: loc.<node-id>.phn -> 127.0.0.1
	// (host-only rewrite, port preserved). Only when mesh node-id is available
	// and no user-defined resolver already matches this source.
	if nodeID != "" {
		loopbackSrc := "loc." + nodeID + ".phn"
		hasLoopback := false
		for _, resolver := range m.resolvers {
			if resolver != nil && resolver.EffectiveSrcHost() == loopbackSrc {
				hasLoopback = true
				break
			}
		}
		if !hasLoopback {
			enabled := true
			m.resolvers = append([]*config.Resolver{{
				Name:    "__LOOPBACK_LOC__",
				Enabled: &enabled,
				SrcHost: loopbackSrc,
				SrcPort: 0,
				DstHost: "127.0.0.1",
				DstPort: 0,
			}}, m.resolvers...)
		}
	}

	// 初始化代理组
	for _, group := range m.groups {
		if !group.IsEnabled() {
			continue
		}
		if err := group.Init(); err != nil {
			return err
		}
	}

	// 检测代理组循环引用
	for _, group := range m.groups {
		if !group.IsEnabled() {
			continue
		}
		if err := m.checkGroupCycle(group.Name, make(map[string]bool)); err != nil {
			return err
		}
	}

	// 注入查找回调
	for _, group := range m.groups {
		g := group
		g.ResolveProxy = func(name string) *config.Proxy {
			return m.proxies[name]
		}
		g.ResolveGroup = func(name string) *config.ProxyGroup {
			return m.groups[name]
		}
		g.ResolveSubscription = func(name string) *config.Subscription {
			return m.subscriptions[name]
		}
	}

	// 验证代理组的订阅引用
	for _, group := range m.groups {
		if !group.IsEnabled() {
			continue
		}
		if group.Subscription != "" {
			if _, ok := m.subscriptions[group.Subscription]; !ok {
				return fmt.Errorf("group %s references unknown subscription: %s", group.Name, group.Subscription)
			}
		}
	}

	// 设置代理链（必须在组注册之后）
	for _, proxy := range m.proxies {
		visited := map[*config.Proxy]bool{}
		if err := m.setNext(proxy, visited); err != nil {
			return err
		}
	}

	// 构建匹配器
	if err := m.buildMatchers(); err != nil {
		return err
	}

	return nil
}

// GetGroup 获取指定的代理组（用于 API 查询）
func (m *RuleMatcher) GetGroup(name string) *config.ProxyGroup {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.groups[name]
}

// AddProxy 添加代理到统一 Map（由 ControlManager 调用）
func (m *RuleMatcher) AddProxy(proxy *config.Proxy) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proxies[proxy.Name] = proxy
	util.LogModuleInfo("db", "added proxy: %s", proxy.Name)
}

// RemoveProxy 从统一 Map 移除代理（由 ControlManager 调用）
func (m *RuleMatcher) RemoveProxy(proxyName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.proxies, proxyName)
	util.LogModuleInfo("db", "removed proxy: %s", proxyName)
}

// AddMapping 添加映射到统一 Map
func (m *RuleMatcher) AddMapping(mapping *config.Mapping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mappings[mapping.Name] = mapping
}

// RemoveMapping 从统一 Map 移除映射
func (m *RuleMatcher) RemoveMapping(mappingName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mappings, mappingName)
}

// AddRule 添加规则
func (m *RuleMatcher) AddRule(rule *config.Rule) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 前置，优先级最高
	m.rules = append([]*config.Rule{rule}, m.rules...)
	// 重建匹配器
	m.buildMatchers()
}

// RemoveRule 移除规则（按代理名）
func (m *RuleMatcher) RemoveRule(proxyName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	newRules := make([]*config.Rule, 0, len(m.rules))
	for _, rule := range m.rules {
		if rule.ProxyName != proxyName {
			newRules = append(newRules, rule)
		}
	}
	m.rules = newRules
	// 重建匹配器
	m.buildMatchers()
}

// buildMatchers 构建匹配器（从 rules）
func (m *RuleMatcher) buildMatchers() error {
	m.matchers = make([]config.Matcher, 0, len(m.rules))
	m.matcherTimeRanges = make([]config.TimeRange, 0, len(m.rules))
	
	for _, rule := range m.rules {
		switch rule.Type {
		case "DOMAIN-SUFFIX":
			matcher := config.NewDomainSuffixMatcher(rule.ProxyName, rule.MappingName, rule.Pattern)
			m.matchers = append(m.matchers, matcher)
			if rule.TimeRange != nil {
				m.matcherTimeRanges = append(m.matcherTimeRanges, *rule.TimeRange)
			} else {
				m.matcherTimeRanges = append(m.matcherTimeRanges, config.TimeRange{})
			}
		case "IP-CIDR":
			matcher := config.NewIpCidMatcher(rule.ProxyName, rule.MappingName, rule.Pattern)
			m.matchers = append(m.matchers, matcher)
			if rule.TimeRange != nil {
				m.matcherTimeRanges = append(m.matcherTimeRanges, *rule.TimeRange)
			} else {
				m.matcherTimeRanges = append(m.matcherTimeRanges, config.TimeRange{})
			}
		case "MATCH":
			matcher := config.NewMatchAllMatcher(rule.ProxyName, rule.MappingName)
			m.matchers = append(m.matchers, matcher)
			if rule.TimeRange != nil {
				m.matcherTimeRanges = append(m.matcherTimeRanges, *rule.TimeRange)
			} else {
				m.matcherTimeRanges = append(m.matcherTimeRanges, config.TimeRange{})
			}
		}
	}
	return nil
}

func (m *RuleMatcher) setNext(proxy *config.Proxy, visited map[*config.Proxy]bool) error {
	next, ok := m.proxies[proxy.ViaProxy]
	if !ok || next == nil {
		// If the name matches a proxy group, resolve it directly.
		if group, gok := m.groups[proxy.ViaProxy]; gok && group != nil {
			next = group.NextWithVisited(make(map[string]bool))
			if next != nil {
				ok = true
			}
		}
	}
	if !ok || next == nil {
		proxy.Next = config.SingletonProxy(config.ProxyDIRECT)
		return nil
	}
	if visited[next] {
		return fmt.Errorf("recycle proxy: %s", proxy.ViaProxy)
	}
	visited[next] = true
	proxy.Next = next
	return m.setNext(next, visited)
}

func (m *RuleMatcher) checkGroupCycle(name string, visited map[string]bool) error {
	if visited[name] {
		return fmt.Errorf("proxy group cycle detected involving %s", name)
	}
	visited[name] = true
	defer delete(visited, name)

	group, ok := m.groups[name]
	if !ok {
		return nil
	}
	for _, member := range group.ManualProxies {
		if _, isGroup := m.groups[member]; isGroup {
			if err := m.checkGroupCycle(member, visited); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *RuleMatcher) findMapping(name string) *config.Mapping {
	if name == "" {
		return nil
	}
	return m.mappings[name]
}


func (m *RuleMatcher) match(request *config.AddrRequest, mapping *config.Mapping) (*config.Proxy, *config.MatchResult) {
	nowMin := time.Now().Hour()*60 + time.Now().Minute()
	util.LogModuleInfo("system", "[MATCH-DEBUG] Starting match: dst=%s:%d mapping=%s nowMin=%d totalMatchers=%d",
		request.DstAddr, request.DstPort, func() string { if mapping != nil { return mapping.Name }; return "<nil>" }(), nowMin, len(m.matchers))
	for i, matcher := range m.matchers {
		if i < len(m.matcherTimeRanges) && !m.matcherTimeRanges[i].Matches(nowMin) {
			util.LogModuleInfo("system", "[MATCH-DEBUG] Rule %d skipped: time range %s doesn't match nowMin=%d", i, m.matcherTimeRanges[i].String(), nowMin)
			continue
		}
		proxyName := matcher.Match(request, mapping)
		if proxyName == "" {
			util.LogModuleInfo("system", "[MATCH-DEBUG] Rule %d: no match (proxyName empty)", i)
			continue
		}
		util.LogModuleInfo("system", "[MATCH-DEBUG] Rule %d matched: proxyName=%s", i, proxyName)
		result := &config.MatchResult{
			ProxyName: proxyName,
		}
		if mapping != nil {
			result.Mapping = mapping.Name
		}
		if i < len(m.matcherTimeRanges) {
			tr := m.matcherTimeRanges[i]
			if tr.StartMin >= 0 {
				result.TimeRange = tr.String()
			}
		}
		resolved := m.resolveName(proxyName)
		util.LogModuleInfo("system", "[MATCH-DEBUG] resolveName(%s) = %v", proxyName, resolved != nil)
		return resolved, result
	}
	util.LogModuleInfo("system", "[MATCH-DEBUG] No rule matched, returning nil")
	return nil, nil
}

func (m *RuleMatcher) resolveName(name string) *config.Proxy {
	if proxy, ok := m.proxies[name]; ok && proxy != nil {
		return proxy
	}
	group, ok := m.groups[name]
	if !ok || len(group.GetMembers()) == 0 {
		return nil
	}
	return group.NextWithVisited(make(map[string]bool))
}

func (m *RuleMatcher) resolving(req *config.AddrRequest) *config.AddrRequest {
	if m.resolvers == nil {
		return req
	}
	for _, resolver := range m.resolvers {
		if !resolver.IsEnabled() {
			continue
		}
		if resolver.Proto != "" && req.Proto != "" && resolver.Proto != req.Proto {
			continue
		}
		if req.DstAddr != resolver.EffectiveSrcHost() {
			continue
		}
		// SrcPort == 0 means "match any port"; otherwise require exact match.
		if resolver.SrcPort != 0 && req.DstPort != resolver.SrcPort {
			continue
		}
		// DstPort == 0 means "keep original port" (host-only rewrite).
		newPort := req.DstPort
		if resolver.DstPort != 0 {
			newPort = resolver.DstPort
		}
		return &config.AddrRequest{
			DstAddr: resolver.DstHost,
			DstPort: newPort,
			CmdType: req.CmdType,
			Proto:   req.Proto,
		}
	}
	return req
}

// ===== 全局缓存和 watcher 机制 =====

var (
	globalMatcher *RuleMatcher
	matcherMu     sync.RWMutex
	watcherID     int64 = -1
)

// GetMatcher 获取当前的 RuleMatcher
func GetMatcher() (*RuleMatcher, error) {
	// 快速路径：读锁获取当前 matcher
	matcherMu.RLock()
	m := globalMatcher
	matcherMu.RUnlock()

	if m != nil {
		return m, nil
	}

	// 慢路径：首次构建
	matcherMu.Lock()
	defer matcherMu.Unlock()

	// 双重检查
	if globalMatcher != nil {
		return globalMatcher, nil
	}

	m, err := BuildRuleMatcher()
	if err != nil {
		return nil, err
	}
	globalMatcher = m

	// 注册 watcher（只需一次）
	if watcherID < 0 {
		watcherID = Subscribe(nil, onConfigChange)
		util.LogModuleInfo("db", "registered RuleMatcher watcher (id=%d)", watcherID)
	}

	return globalMatcher, nil
}

// onConfigChange 处理 DB 变更事件，直接更新 Map
func onConfigChange(event ChangeEvent) {
	matcherMu.RLock()
	m := globalMatcher
	matcherMu.RUnlock()

	if m == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	bucket := string(event.Bucket)
	key := event.Key
	op := string(event.Op)

	switch bucket {
	case string(BucketProxies):
		if op == "delete" {
			delete(m.proxies, key)
			util.LogModuleInfo("db", "watch: removed proxy %s", key)
		} else {
			// Insert or update
			proxy, err := GetProxy(key)
			if err != nil || proxy == nil {
				util.LogModuleError("db", "watch: failed to load proxy %s: %v", key, err)
				return
			}
			m.proxies[key] = proxy
			util.LogModuleInfo("db", "watch: updated proxy %s", key)
		}
		// 代理变更不需要重建成员列表，因为 Members 是按需计算的

	case string(BucketProxyGroups):
		if op == "delete" {
			delete(m.groups, key)
			util.LogModuleInfo("db", "watch: removed group %s", key)
		} else {
			group, err := GetProxyGroup(key)
			if err != nil || group == nil {
				util.LogModuleError("db", "watch: failed to load group %s: %v", key, err)
				return
			}
			// 注入查找回调（与 init() 保持一致）
			group.ResolveProxy = func(name string) *config.Proxy {
				return m.proxies[name]
			}
			group.ResolveGroup = func(name string) *config.ProxyGroup {
				return m.groups[name]
			}
			group.ResolveSubscription = func(name string) *config.Subscription {
				return m.subscriptions[name]
			}
			m.groups[key] = group
			util.LogModuleInfo("db", "watch: updated group %s", key)
		}

	case string(BucketSubscriptions):
		if op == "delete" {
			delete(m.subscriptions, key)
			util.LogModuleInfo("db", "watch: removed subscription %s", key)
		} else {
			sub, err := GetSubscription(key)
			if err != nil || sub == nil {
				util.LogModuleError("db", "watch: failed to load subscription %s: %v", key, err)
				return
			}
			// 加载节点池
			nodes, err := GetSubscriptionNodes(key)
			if err == nil && nodes != nil {
				sub.SubMu.Lock()
				sub.SubProxies = nodes
				sub.SubMu.Unlock()
			}
			m.subscriptions[key] = sub
			util.LogModuleInfo("db", "watch: updated subscription %s", key)
		}
		// 订阅变更不需要重建成员列表，因为 Members 是按需计算的

	case string(BucketSubscriptionNodes):
		// 节点池变更，更新订阅的 SubProxies
		if sub, ok := m.subscriptions[key]; ok {
			nodes, err := GetSubscriptionNodes(key)
			if err == nil && nodes != nil {
				sub.SubMu.Lock()
				sub.SubProxies = nodes
				sub.SubMu.Unlock()
				util.LogModuleInfo("db", "watch: updated subscription nodes for %s", key)
			}
			// 订阅节点变更不需要重建成员列表，因为 Members 是按需计算的
		}

	case string(BucketMappings):
		if op == "delete" {
			delete(m.mappings, key)
			util.LogModuleInfo("db", "watch: removed mapping %s", key)
		} else {
			mapping, err := GetMapping(key)
			if err != nil || mapping == nil {
				util.LogModuleError("db", "watch: failed to load mapping %s: %v", key, err)
				return
			}
			m.mappings[key] = mapping
			util.LogModuleInfo("db", "watch: updated mapping %s", key)
		}

	case string(BucketRules):
		// 规则变更，重新加载所有规则
		ruleStrs, err := listRules()
		if err != nil {
			util.LogModuleError("db", "watch: failed to load rules: %v", err)
			return
		}
		m.rules = parseRules(ruleStrs)
		m.buildMatchers()
		util.LogModuleInfo("db", "watch: updated rules (%d rules)", len(m.rules))
	}
}

// ===== 便捷接口 =====

// HasReverseAddress checks if any enabled proxy has the given reverse address.
func HasReverseAddress(addr string) bool {
	proxies, err := ListProxies()
	if err != nil {
		return false
	}
	for _, p := range proxies {
		if p.IsEnabled() && p.ReverseAddress == addr {
			return true
		}
	}
	return false
}

// ResolveMatch 从数据库实时匹配
func ResolveMatch(req *config.AddrRequest, mappingName string) (
	*config.AddrRequest, *config.Proxy, *config.MatchResult, error) {

	m, err := GetMatcher()
	if err != nil {
		return nil, nil, nil, err
	}

	return m.ResolveMatch(req, mappingName)
}
