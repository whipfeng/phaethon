package db

import (
	"fmt"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"phaethon/config"
	"phaethon/util"
)

// RuleMatcher 是专门的规则匹配器
// 只包含规则匹配需要的数据，不包含 Admin/TUN/ReverseConfigs 等无关配置
type RuleMatcher struct {
	conf *config.RuleConfiguration
}

// BuildRuleMatcher 从数据库构建 RuleMatcher
func BuildRuleMatcher() (*RuleMatcher, error) {
	m := &RuleMatcher{
		conf: &config.RuleConfiguration{},
	}

	// 从数据库加载规则匹配需要的数据
	err := View(func(tx *bolt.Tx) error {
		var err error

		// 加载代理
		if m.conf.Proxies, err = listTx[config.Proxy](tx, BucketProxies); err != nil {
			return fmt.Errorf("load proxies: %w", err)
		}

		// 加载代理组
		if m.conf.ProxyGroups, err = listTx[config.ProxyGroup](tx, BucketProxyGroups); err != nil {
			return fmt.Errorf("load proxy groups: %w", err)
		}

		// 加载订阅（元数据 + 节点池）
		if m.conf.Subscriptions, err = listTx[config.Subscription](tx, BucketSubscriptions); err != nil {
			return fmt.Errorf("load subscriptions: %w", err)
		}
		// 为每个订阅加载节点池到 SubProxies
		for i := range m.conf.Subscriptions {
			sub := m.conf.Subscriptions[i]
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
		if m.conf.Rules, err = listRulesTx(tx); err != nil {
			return fmt.Errorf("load rules: %w", err)
		}

		// 加载映射
		if m.conf.Mappings, err = listTx[config.Mapping](tx, BucketMappings); err != nil {
			return fmt.Errorf("load mappings: %w", err)
		}

		// 加载解析器
		if m.conf.Resolvers, err = listTx[config.Resolver](tx, BucketResolvers); err != nil {
			return fmt.Errorf("load resolvers: %w", err)
		}

		// 加载 Mesh 配置（仅用于 resolver 的 src-relative）
		cb := tx.Bucket(BucketConfig)
		if cb != nil {
			var mesh config.MeshConfig
			if present, err := readScalarInto(cb, keyMesh, &mesh); err != nil {
				return err
			} else if present {
				m.conf.Mesh = &mesh
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// 调用 Init() 构建对象关系
	if err := m.conf.Init(); err != nil {
		return nil, fmt.Errorf("config init: %w", err)
	}

	return m, nil
}

// ResolveMatch 执行规则匹配
func (m *RuleMatcher) ResolveMatch(req *config.AddrRequest, mappingName string) (
	*config.AddrRequest, *config.Proxy, *config.MatchResult, error) {

	var mapping *config.Mapping
	if mappingName != "" {
		mapping = m.conf.FindMapping(mappingName)
	}

	newReq, proxy, matchResult := m.conf.ResolveMatch(req, mapping)
	return newReq, proxy, matchResult, nil
}

// ===== 全局缓存和 watcher 机制 =====

var (
	globalMatcher *RuleMatcher
	matcherMu     sync.RWMutex
	matcherBuilt  bool
	watcherID     int64 = -1
)

// GetMatcher 获取当前的 RuleMatcher
func GetMatcher() (*RuleMatcher, error) {
	matcherMu.RLock()
	if matcherBuilt {
		m := globalMatcher
		matcherMu.RUnlock()
		return m, nil
	}
	matcherMu.RUnlock()

	matcherMu.Lock()
	defer matcherMu.Unlock()

	if !matcherBuilt {
		m, err := BuildRuleMatcher()
		if err != nil {
			return nil, err
		}
		globalMatcher = m
		matcherBuilt = true
		registerMatcherWatcher()
	}

	return globalMatcher, nil
}

// registerMatcherWatcher 注册数据库变更监听器
func registerMatcherWatcher() {
	if watcherID >= 0 {
		return
	}
	watcherID = Subscribe(nil, rebuildMatcher)
	util.LogModuleInfo("db", "registered RuleMatcher watcher (id=%d)", watcherID)
}

// rebuildMatcher 重建 RuleMatcher（带 debounce）
var rebuildTimer *time.Timer
var rebuildMu sync.Mutex

func rebuildMatcher(event ChangeEvent) {
	bucket := string(event.Bucket)
	if bucket != string(BucketRules) &&
		bucket != string(BucketProxies) &&
		bucket != string(BucketProxyGroups) &&
		bucket != string(BucketSubscriptions) &&
		bucket != string(BucketMappings) &&
		bucket != string(BucketResolvers) &&
		bucket != string(BucketConfig) {
		return
	}

	rebuildMu.Lock()
	if rebuildTimer != nil {
		rebuildTimer.Stop()
	}
	rebuildTimer = time.AfterFunc(500*time.Millisecond, func() {
		doRebuildMatcher(bucket)
	})
	rebuildMu.Unlock()
}

func doRebuildMatcher(triggerBucket string) {
	util.LogModuleInfo("db", "rebuilding RuleMatcher due to %s change", triggerBucket)

	m, err := BuildRuleMatcher()
	if err != nil {
		util.LogModuleError("db", "rebuild RuleMatcher failed: %v", err)
		return
	}

	matcherMu.Lock()
	globalMatcher = m
	matcherMu.Unlock()

	util.LogModuleInfo("db", "RuleMatcher rebuilt successfully")
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
