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

// RuleMatcher 是专门的规则匹配器
// 只包含规则匹配需要的数据，不包含 Admin/TUN/ReverseConfigs 等无关配置
type RuleMatcher struct {
	// 静态配置（从 DB 加载，不可变）
	proxies       []*config.Proxy
	proxyGroups   []*config.ProxyGroup
	subscriptions []*config.Subscription
	rules         []string
	mappings      []*config.Mapping
	resolvers     []*config.Resolver
	mesh          *config.MeshConfig

	// 动态配置（运行时注入，反向连接注册）
	dynamicProxies  []*config.Proxy
	dynamicRules    []string
	dynamicMatchers []config.Matcher
	dynamicTimeRanges []config.TimeRange

	// Initialized fields (built from above data)
	proxyNames        map[string]*config.Proxy
	groupNames        map[string]*config.ProxyGroup
	subscriptionNames map[string]*config.Subscription
	matchers          []config.Matcher
	matcherTimeRanges []config.TimeRange
	matcherRules      []string

	// 保护动态配置的并发访问
	dynamicMu sync.RWMutex
}

// BuildRuleMatcher 从数据库构建 RuleMatcher
func BuildRuleMatcher() (*RuleMatcher, error) {
	m := &RuleMatcher{}

	// 从数据库加载规则匹配需要的数据
	err := View(func(tx *bolt.Tx) error {
		var err error

		// 加载代理
		if m.proxies, err = listTx[config.Proxy](tx, BucketProxies); err != nil {
			return fmt.Errorf("load proxies: %w", err)
		}

		// 加载代理组
		if m.proxyGroups, err = listTx[config.ProxyGroup](tx, BucketProxyGroups); err != nil {
			return fmt.Errorf("load proxy groups: %w", err)
		}

		// 加载订阅（元数据 + 节点池）
		if m.subscriptions, err = listTx[config.Subscription](tx, BucketSubscriptions); err != nil {
			return fmt.Errorf("load subscriptions: %w", err)
		}
		// 为每个订阅加载节点池到 SubProxies
		for i := range m.subscriptions {
			sub := m.subscriptions[i]
			nodes, err := GetSubscriptionNodes(sub.Name)
			if err != nil {
				return fmt.Errorf("load subscription nodes for %s: %w", sub.Name, err)
			}
			if nodes != nil {
				sub.SubMu.Lock()
				sub.SubProxies = nodes
				sub.SubMu.Unlock()
			}
		}

		// 加载规则
		if m.rules, err = listRulesTx(tx); err != nil {
			return fmt.Errorf("load rules: %w", err)
		}

		// 加载映射
		if m.mappings, err = listTx[config.Mapping](tx, BucketMappings); err != nil {
			return fmt.Errorf("load mappings: %w", err)
		}

		// 加载解析器
		if m.resolvers, err = listTx[config.Resolver](tx, BucketResolvers); err != nil {
			return fmt.Errorf("load resolvers: %w", err)
		}

		// 加载 Mesh 配置（仅用于 resolver 的 src-relative）
		cb := tx.Bucket(BucketConfig)
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

// ResolveMatch 执行规则匹配
func (m *RuleMatcher) ResolveMatch(req *config.AddrRequest, mappingName string) (
	*config.AddrRequest, *config.Proxy, *config.MatchResult, error) {

	// Apply resolver rules first
	req = m.resolving(req)

	var mapping *config.Mapping
	if mappingName != "" {
		mapping = m.findMapping(mappingName)
	}

	proxy, matchResult := m.match(req, mapping)
	return req, proxy, matchResult, nil
}

// init builds internal lookup maps and matchers from loaded data
func (m *RuleMatcher) init() error {
	m.proxyNames = make(map[string]*config.Proxy)
	m.groupNames = make(map[string]*config.ProxyGroup)
	m.subscriptionNames = make(map[string]*config.Subscription)
	m.proxyNames[config.ProxyDIRECT] = config.SingletonProxy(config.ProxyDIRECT)
	m.proxyNames[config.ProxyREJECT] = config.SingletonProxy(config.ProxyREJECT)

	// Register proxies
	for _, proxy := range m.proxies {
		if !proxy.IsEnabled() {
			continue
		}
		proxy.Type = strings.ToLower(proxy.Type)
		if _, exists := m.proxyNames[proxy.Name]; exists {
			return fmt.Errorf("proxyName conflict: %s", proxy.Name)
		}
		if _, exists := m.groupNames[proxy.Name]; exists {
			return fmt.Errorf("proxyName conflict with group: %s", proxy.Name)
		}
		if _, exists := m.subscriptionNames[proxy.Name]; exists {
			return fmt.Errorf("proxyName conflict with subscription: %s", proxy.Name)
		}
		m.proxyNames[proxy.Name] = proxy

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

	// Register subscriptions before groups so groups can reference them.
	for _, sub := range m.subscriptions {
		if !sub.IsEnabled() {
			continue
		}
		if _, exists := m.proxyNames[sub.Name]; exists {
			return fmt.Errorf("subscription name conflict with proxy: %s", sub.Name)
		}
		if _, exists := m.groupNames[sub.Name]; exists {
			return fmt.Errorf("subscription name conflict with group: %s", sub.Name)
		}
		if _, exists := m.subscriptionNames[sub.Name]; exists {
			return fmt.Errorf("subscription name conflict: %s", sub.Name)
		}
		if sub.SubProxies == nil {
			sub.SubProxies = make(map[string]*config.Proxy)
		}
		m.subscriptionNames[sub.Name] = sub
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

	// Register proxy groups FIRST so setNext can resolve proxy chains
	for _, group := range m.proxyGroups {
		if !group.IsEnabled() {
			continue
		}
		if err := group.Init(); err != nil {
			return err
		}
		if _, exists := m.proxyNames[group.Name]; exists {
			return fmt.Errorf("groupName conflict with proxy: %s", group.Name)
		}
		if _, exists := m.groupNames[group.Name]; exists {
			return fmt.Errorf("groupName conflict: %s", group.Name)
		}
		if _, exists := m.subscriptionNames[group.Name]; exists {
			return fmt.Errorf("groupName conflict with subscription: %s", group.Name)
		}
		m.groupNames[group.Name] = group
	}

	// Detect cyclic group references before callbacks are injected.
	for _, group := range m.proxyGroups {
		if !group.IsEnabled() {
			continue
		}
		if err := m.checkGroupCycle(group.Name, make(map[string]bool)); err != nil {
			return err
		}
	}

	// Inject lookup callbacks into each group.
	for _, group := range m.proxyGroups {
		g := group
		g.ResolveProxy = func(name string) *config.Proxy {
			return m.proxyNames[name]
		}
		g.ResolveGroup = func(name string) *config.ProxyGroup {
			return m.groupNames[name]
		}
		g.ResolveSubscription = func(name string) *config.Subscription {
			return m.subscriptionNames[name]
		}
	}

	// Rebuild runtime member lists for all groups now that lookup callbacks are in place.
	for _, group := range m.proxyGroups {
		group.RebuildProxies()
	}

	// Validate group subscription references.
	for _, group := range m.proxyGroups {
		if !group.IsEnabled() {
			continue
		}
		if group.Subscription != "" {
			if _, ok := m.subscriptionNames[group.Subscription]; !ok {
				return fmt.Errorf("group %s references unknown subscription: %s", group.Name, group.Subscription)
			}
		}
	}

	// Set proxy chains (must run after groups are registered)
	for _, proxy := range m.proxies {
		visited := map[*config.Proxy]bool{}
		if err := m.setNext(proxy, visited); err != nil {
			return err
		}
	}

	// Build lookup tables (static configs only, dynamic configs added later via AddDynamicConfig)
	// Note: rebuildLookupTablesLocked requires dynamicMu to be held
	m.dynamicMu.Lock()
	m.rebuildLookupTablesLocked()
	m.dynamicMu.Unlock()

	return nil
}

// AddDynamicConfig 注入动态配置（由 ControlManager 调用）
// 动态配置会插入到匹配列表的最前面，优先于静态配置
func (m *RuleMatcher) AddDynamicConfig(proxy *config.Proxy, rule string, matcher config.Matcher, tr config.TimeRange) {
	m.dynamicMu.Lock()
	defer m.dynamicMu.Unlock()

	// 添加到动态配置列表
	m.dynamicProxies = append(m.dynamicProxies, proxy)
	m.dynamicRules = append(m.dynamicRules, rule)
	m.dynamicMatchers = append([]config.Matcher{matcher}, m.dynamicMatchers...) // 前置
	m.dynamicTimeRanges = append([]config.TimeRange{tr}, m.dynamicTimeRanges...) // 前置

	// 重建查找表（包含动态配置）
	m.rebuildLookupTablesLocked()

	util.LogModuleInfo("db", "added dynamic config: proxy=%s rule=%s", proxy.Name, rule)
}

// RemoveDynamicConfig 移除动态配置（由 ControlManager 调用）
func (m *RuleMatcher) RemoveDynamicConfig(proxyName string) {
	m.dynamicMu.Lock()
	defer m.dynamicMu.Unlock()

	// 查找并移除
	for i, p := range m.dynamicProxies {
		if p.Name == proxyName {
			// 移除 proxy
			m.dynamicProxies = append(m.dynamicProxies[:i], m.dynamicProxies[i+1:]...)
			// 移除对应的 rule、matcher、timeRange（索引相同）
			if i < len(m.dynamicRules) {
				m.dynamicRules = append(m.dynamicRules[:i], m.dynamicRules[i+1:]...)
			}
			if i < len(m.dynamicMatchers) {
				m.dynamicMatchers = append(m.dynamicMatchers[:i], m.dynamicMatchers[i+1:]...)
			}
			if i < len(m.dynamicTimeRanges) {
				m.dynamicTimeRanges = append(m.dynamicTimeRanges[:i], m.dynamicTimeRanges[i+1:]...)
			}
			// 重建查找表
			m.rebuildLookupTablesLocked()
			util.LogModuleInfo("db", "removed dynamic config: proxy=%s", proxyName)
			return
		}
	}
}

// rebuildLookupTablesLocked 重建查找表（包含静态+动态配置）
// 调用者必须持有 dynamicMu.Lock()
func (m *RuleMatcher) rebuildLookupTablesLocked() {
	// ============ 合并 proxyNames ============
	// 静态 proxies
	mergedProxyNames := make(map[string]*config.Proxy)
	mergedProxyNames[config.ProxyDIRECT] = config.SingletonProxy(config.ProxyDIRECT)
	mergedProxyNames[config.ProxyREJECT] = config.SingletonProxy(config.ProxyREJECT)

	for _, proxy := range m.proxies {
		if !proxy.IsEnabled() {
			continue
		}
		mergedProxyNames[proxy.Name] = proxy
	}
	// 动态 proxies（覆盖静态，如果同名）
	for _, proxy := range m.dynamicProxies {
		if !proxy.IsEnabled() {
			continue
		}
		mergedProxyNames[proxy.Name] = proxy
	}
	m.proxyNames = mergedProxyNames

	// ============ 合并 matchers ============
	// 动态 matchers 在前，静态 matchers 在后
	m.matchers = append(m.dynamicMatchers, m.buildStaticMatchers()...)

	// ============ 合并 time ranges ============
	m.matcherTimeRanges = append(m.dynamicTimeRanges, m.buildStaticTimeRanges()...)

	// ============ 合并 rules ============
	m.matcherRules = append(m.dynamicRules, m.buildStaticRules()...)
}

// buildStaticMatchers 构建静态 matchers（从 m.rules）
func (m *RuleMatcher) buildStaticMatchers() []config.Matcher {
	matchers := make([]config.Matcher, 0, len(m.rules))
	for _, rule := range m.rules {
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
			name, mapping, _ := config.ParseProxyName(strings.Trim(rs[2], `"'`))
			matchers = append(matchers, config.NewDomainSuffixMatcher(name, mapping, strings.Trim(rs[1], `"'`)))
		case "IP-CIDR":
			if len(rs) < 3 {
				continue
			}
			name, mapping, _ := config.ParseProxyName(strings.Trim(rs[2], `"'`))
			matchers = append(matchers, config.NewIpCidMatcher(name, mapping, strings.Trim(rs[1], `"'`)))
		case "MATCH":
			name, mapping, _ := config.ParseProxyName(strings.Trim(rs[1], `"'`))
			matchers = append(matchers, config.NewMatchAllMatcher(name, mapping))
		}
	}
	return matchers
}

// buildStaticTimeRanges 构建静态 time ranges
func (m *RuleMatcher) buildStaticTimeRanges() []config.TimeRange {
	trs := make([]config.TimeRange, 0, len(m.rules))
	for _, rule := range m.rules {
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
			_, _, tr := config.ParseProxyName(strings.Trim(rs[2], `"'`))
			trs = append(trs, tr)
		case "IP-CIDR":
			if len(rs) < 3 {
				continue
			}
			_, _, tr := config.ParseProxyName(strings.Trim(rs[2], `"'`))
			trs = append(trs, tr)
		case "MATCH":
			_, _, tr := config.ParseProxyName(strings.Trim(rs[1], `"'`))
			trs = append(trs, tr)
		}
	}
	return trs
}

// buildStaticRules 构建静态 rules（过滤后的原始规则串）
func (m *RuleMatcher) buildStaticRules() []string {
	rules := make([]string, 0, len(m.rules))
	for _, rule := range m.rules {
		trimmed := strings.TrimSpace(rule)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		rs := strings.SplitN(rule, ",", 3)
		if len(rs) < 2 {
			continue
		}
		valid := false
		switch rs[0] {
		case "DOMAIN-SUFFIX":
			valid = len(rs) >= 3
		case "IP-CIDR":
			valid = len(rs) >= 3
		case "MATCH":
			valid = len(rs) >= 2
		}
		if valid {
			rules = append(rules, rule)
		}
	}
	return rules
}

func (m *RuleMatcher) setNext(proxy *config.Proxy, visited map[*config.Proxy]bool) error {
	next, ok := m.proxyNames[proxy.ViaProxy]
	if !ok || next == nil {
		// If the name matches a proxy group, resolve it directly.
		if group, gok := m.groupNames[proxy.ViaProxy]; gok && group != nil {
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

	group, ok := m.groupNames[name]
	if !ok {
		return nil
	}
	for _, member := range group.ManualProxies {
		if _, isGroup := m.groupNames[member]; isGroup {
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
	for _, mapping := range m.mappings {
		if mapping.Name == name {
			return mapping
		}
	}
	return nil
}

func (m *RuleMatcher) match(request *config.AddrRequest, mapping *config.Mapping) (*config.Proxy, *config.MatchResult) {
	nowMin := time.Now().Hour()*60 + time.Now().Minute()
	for i, matcher := range m.matchers {
		if i < len(m.matcherTimeRanges) && !m.matcherTimeRanges[i].Matches(nowMin) {
			continue
		}
		proxyName := matcher.Match(request, mapping)
		if proxyName == "" {
			continue
		}
		result := &config.MatchResult{
			ProxyName: proxyName,
		}
		if i < len(m.matcherRules) {
			result.Rule = m.matcherRules[i]
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
		return m.resolveName(proxyName), result
	}
	return nil, nil
}

func (m *RuleMatcher) resolveName(name string) *config.Proxy {
	if proxy, ok := m.proxyNames[name]; ok && proxy != nil {
		return proxy
	}
	group, ok := m.groupNames[name]
	if !ok || len(group.Members) == 0 {
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
		if req.DstAddr != resolver.EffectiveSrcHost() || req.DstPort != resolver.SrcPort {
			continue
		}
		return &config.AddrRequest{
			DstAddr: resolver.DstHost,
			DstPort: resolver.DstPort,
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

	// 重建控制：channel 串行化，避免并发重建
	rebuildCh = make(chan string, 1) // buffer=1，非阻塞发送

	// RebuildHook 是重建后调用的回调函数（用于关闭控制连接等）
	// 由 main.go 注册，避免 db → server 的循环依赖
	RebuildHook func()
)

func init() {
	go rebuildLoop()
}

// rebuildLoop 串行处理重建请求（单 goroutine）
func rebuildLoop() {
	for triggerBucket := range rebuildCh {
		// 直接重建，channel 串行化已防止并发
		doRebuildMatcher(triggerBucket)
	}
}

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
		watcherID = Subscribe(nil, rebuildMatcher)
		util.LogModuleInfo("db", "registered RuleMatcher watcher (id=%d)", watcherID)
	}

	return globalMatcher, nil
}

// shouldRebuildBucket 判断是否需要触发重建
func shouldRebuildBucket(bucket string) bool {
	return bucket == string(BucketRules) ||
		bucket == string(BucketProxies) ||
		bucket == string(BucketProxyGroups) ||
		bucket == string(BucketSubscriptions) ||
		bucket == string(BucketSubscriptionNodes) || // 节点池变更
		bucket == string(BucketMappings) ||
		bucket == string(BucketResolvers) ||
		bucket == string(BucketConfig)
}

// rebuildMatcher 由 watcher 调用，触发重建（非阻塞）
func rebuildMatcher(event ChangeEvent) {
	bucket := string(event.Bucket)

	if !shouldRebuildBucket(bucket) {
		return
	}

	// 非阻塞发送触发信号
	select {
	case rebuildCh <- bucket:
		util.LogModuleInfo("db", "triggered rebuild for %s", bucket)
	default:
		// 已有重建待处理，跳过
		util.LogModuleInfo("db", "rebuild already pending, skipping %s", bucket)
	}
}

// doRebuildMatcher 执行重建（旧配置继续服务）
func doRebuildMatcher(triggerBucket string) {
	util.LogModuleInfo("db", "rebuilding RuleMatcher due to %s change", triggerBucket)

	// 1. 构建新 matcher（不持锁，旧 matcher 继续服务）
	m, err := BuildRuleMatcher()
	if err != nil {
		util.LogModuleError("db", "rebuild RuleMatcher failed: %v", err)
		return
	}

	// 2. 原子替换（短暂持写锁）
	matcherMu.Lock()
	globalMatcher = m
	matcherMu.Unlock()

	util.LogModuleInfo("db", "RuleMatcher rebuilt successfully")

	// 3. 最后调用回调（关闭控制连接，让反向连接重连并注入动态配置）
	if RebuildHook != nil {
		RebuildHook()
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
