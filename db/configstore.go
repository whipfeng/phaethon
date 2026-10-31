package db

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"phaethon/config"
)

// 拆分后各标量配置在其专属 bucket 内使用统一的 key
var (
	keyAdmin        = []byte("admin")
	keyTUN          = []byte("tun")
	keyMesh         = []byte("mesh")
	keyInteractive  = []byte("interactive")
	keyUDPPortRange = []byte("udp-port-range")
	keyTCPPortRange = []byte("tcp-port-range")
)

// KeyRebuilt 是 ReplaceAllConfig 提交后发出的合成事件 key，
// 重建 watcher 收到即全量重建 matcher。
const KeyRebuilt = "__rebuilt__"

// ===== 事务内 JSON 读写（调用方负责事务生命周期）=====

func putJSON(b *bolt.Bucket, key []byte, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("序列化 %s 失败: %w", key, err)
	}
	return b.Put(key, data)
}

// getTx 读取单条记录。返回的指针是 JSON 反序列化产物，
// 不引用 bolt mmap 内存，事务结束后仍可安全使用。
func getTx[T any](tx *bolt.Tx, bucket, key []byte) (*T, error) {
	b := tx.Bucket(bucket)
	if b == nil {
		return nil, fmt.Errorf("bucket %s 不存在", string(bucket))
	}
	data := b.Get(key)
	if data == nil {
		return nil, ErrNotFound
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("解析 %s/%s 失败: %w", string(bucket), key, err)
	}
	return &v, nil
}

func listTx[T any](tx *bolt.Tx, bucket []byte) ([]*T, error) {
	b := tx.Bucket(bucket)
	if b == nil {
		return nil, fmt.Errorf("bucket %s 不存在", string(bucket))
	}
	var out []*T
	err := b.ForEach(func(k, v []byte) error {
		var e T
		if err := json.Unmarshal(v, &e); err != nil {
			return fmt.Errorf("解析 %s/%s 失败: %w", string(bucket), k, err)
		}
		out = append(out, &e)
		return nil
	})
	return out, err
}

// getView / listView 是单 key 与全量遍历的快捷方法，
// 各自用独立的只读事务包裹（事务在 View 返回时释放）。
func getView[T any](bucket, key []byte) (*T, error) {
	var out *T
	err := View(func(tx *bolt.Tx) error {
		v, err := getTx[T](tx, bucket, key)
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

func listView[T any](bucket []byte) ([]*T, error) {
	var out []*T
	err := View(func(tx *bolt.Tx) error {
		v, err := listTx[T](tx, bucket)
		if err != nil {
			return err
		}
		out = v
		return nil
	})
	return out, err
}

// ===== 标量配置节 =====

func PutAdminConfig(c *config.AdminConfig) error {
	return Put(BucketAdminConfig, keyAdmin, c)
}

func GetAdminConfig() (*config.AdminConfig, error) {
	return getView[config.AdminConfig](BucketAdminConfig, keyAdmin)
}

func PutTUNConfig(c *config.TUNConfig) error {
	return Put(BucketTUNConfig, keyTUN, c)
}

func GetTUNConfig() (*config.TUNConfig, error) {
	return getView[config.TUNConfig](BucketTUNConfig, keyTUN)
}

func PutMeshConfig(c *config.MeshConfig) error {
	return Put(BucketMeshConfig, keyMesh, c)
}

func GetMeshConfig() (*config.MeshConfig, error) {
	return getView[config.MeshConfig](BucketMeshConfig, keyMesh)
}

func PutInteractive(v bool) error {
	return Put(BucketInteractiveConfig, keyInteractive, v)
}

func GetInteractive() (bool, error) {
	v, err := getView[bool](BucketInteractiveConfig, keyInteractive)
	if err != nil {
		return false, err
	}
	return *v, nil
}

func PutUDPPortRange(s string) error {
	return Put(BucketPortRangeConfig, keyUDPPortRange, s)
}

func GetUDPPortRange() (string, error) {
	v, err := getView[string](BucketPortRangeConfig, keyUDPPortRange)
	if err != nil {
		return "", err
	}
	return *v, nil
}

func PutTCPPortRange(s string) error {
	return Put(BucketPortRangeConfig, keyTCPPortRange, s)
}

func GetTCPPortRange() (string, error) {
	v, err := getView[string](BucketPortRangeConfig, keyTCPPortRange)
	if err != nil {
		return "", err
	}
	return *v, nil
}

// ===== 代理 =====

func PutProxy(p *config.Proxy) error {
	return Put(BucketProxies, []byte(p.Name), p)
}

func GetProxy(name string) (*config.Proxy, error) {
	return getView[config.Proxy](BucketProxies, []byte(name))
}

func DeleteProxy(name string) error {
	return Delete(BucketProxies, []byte(name))
}

func ListProxies() ([]*config.Proxy, error) {
	return listView[config.Proxy](BucketProxies)
}

// ===== 代理组 =====

// storeGroup 返回可持久化的代理组副本，清零运行时派生字段。
// 不能整体拷贝结构体：内含 sync.RWMutex（go vet copylocks 会报错）。
func storeGroup(g *config.ProxyGroup) *config.ProxyGroup {
	return &config.ProxyGroup{
		Name:                 g.Name,
		Enabled:              g.Enabled,
		Type:                 g.Type,
		ManualProxies:        append([]string(nil), g.ManualProxies...),
		HealthCheckURL:       g.HealthCheckURL,
		HealthCheckInterval:  g.HealthCheckInterval,
		HealthCheckTolerance: g.HealthCheckTolerance,
		LBStrategy:           g.LBStrategy,
		Subscription:         g.Subscription,
		SubscriptionFilter:   g.SubscriptionFilter,
		ActiveMember:         g.ActiveMember,
	}
}

func PutProxyGroup(g *config.ProxyGroup) error {
	return Put(BucketProxyGroups, []byte(g.Name), storeGroup(g))
}

func GetProxyGroup(name string) (*config.ProxyGroup, error) {
	return getView[config.ProxyGroup](BucketProxyGroups, []byte(name))
}

func DeleteProxyGroup(name string) error {
	return Delete(BucketProxyGroups, []byte(name))
}

func ListProxyGroups() ([]*config.ProxyGroup, error) {
	groups, err := listView[config.ProxyGroup](BucketProxyGroups)
	if err != nil {
		return nil, err
	}
	// Inject resolver callbacks so GetMembers() can compute member lists
	injectGroupResolvers(groups)
	return groups, nil
}

// injectGroupResolvers sets the ResolveProxy/ResolveGroup/ResolveSubscription
// callbacks on proxy groups so they can resolve member references.
func injectGroupResolvers(groups []*config.ProxyGroup) {
	// Build lookup maps
	proxies, _ := ListProxies()
	proxyMap := make(map[string]*config.Proxy, len(proxies))
	for _, p := range proxies {
		proxyMap[p.Name] = p
	}

	groupMap := make(map[string]*config.ProxyGroup, len(groups))
	for _, g := range groups {
		groupMap[g.Name] = g
	}

	subs, _ := ListSubscriptions()
	subMap := make(map[string]*config.Subscription, len(subs))
	for _, s := range subs {
		subMap[s.Name] = s
	}

	// Inject resolvers
	for _, g := range groups {
		g.ResolveProxy = func(name string) *config.Proxy {
			return proxyMap[name]
		}
		g.ResolveGroup = func(name string) *config.ProxyGroup {
			return groupMap[name]
		}
		g.ResolveSubscription = func(name string) *config.Subscription {
			return subMap[name]
		}
	}
}

// ListProxyGroupsBySubscription returns all proxy groups that reference the given subscription
func ListProxyGroupsBySubscription(subName string) ([]*config.ProxyGroup, error) {
	allGroups, err := ListProxyGroups()
	if err != nil {
		return nil, err
	}
	var result []*config.ProxyGroup
	for _, g := range allGroups {
		if g.Subscription == subName {
			result = append(result, g)
		}
	}
	return result, nil
}

// ===== 订阅 =====

func PutSubscription(s *config.Subscription) error {
	return Put(BucketSubscriptions, []byte(s.Name), s)
}

func GetSubscription(name string) (*config.Subscription, error) {
	return getView[config.Subscription](BucketSubscriptions, []byte(name))
}

func DeleteSubscription(name string) error {
	return Delete(BucketSubscriptions, []byte(name))
}

func ListSubscriptions() ([]*config.Subscription, error) {
	return listView[config.Subscription](BucketSubscriptions)
}

// ===== 映射 =====

func PutMapping(m *config.Mapping) error {
	return Put(BucketMappings, []byte(m.Name), m)
}

func GetMapping(name string) (*config.Mapping, error) {
	return getView[config.Mapping](BucketMappings, []byte(name))
}

func DeleteMapping(name string) error {
	return Delete(BucketMappings, []byte(name))
}

func ListMappings() ([]*config.Mapping, error) {
	return listView[config.Mapping](BucketMappings)
}

// ===== 解析器 =====

func PutResolver(r *config.Resolver) error {
	return Put(BucketResolvers, []byte(r.Name), r)
}

func GetResolver(name string) (*config.Resolver, error) {
	return getView[config.Resolver](BucketResolvers, []byte(name))
}

func DeleteResolver(name string) error {
	return Delete(BucketResolvers, []byte(name))
}

func ListResolvers() ([]*config.Resolver, error) {
	return listView[config.Resolver](BucketResolvers)
}

// ===== 反向配置 =====

// storeReverse 清零反向配置的运行时信息字段。
func storeReverse(rc *config.ReverseConfig) *config.ReverseConfig {
	c := *rc
	c.LastError = ""
	c.AssignedPort = 0
	return &c
}

func PutReverseConfig(rc *config.ReverseConfig) error {
	return Put(BucketReverseConfs, []byte(rc.Name), storeReverse(rc))
}

// PutReverseRuntime persists only the runtime-mutable fields (AssignedPort,
// LastError) of a reverse config. The admin UI reads these to display the
// current registration state; without persistence, the UI would always show
// "未启用" because the in-memory updates made by the reverse client goroutine
// are invisible to the DB-backed admin API.
func PutReverseRuntime(rc *config.ReverseConfig) error {
	existing, err := GetReverseConfig(rc.Name)
	if err != nil {
		return err
	}
	existing.AssignedPort = rc.AssignedPort
	existing.LastError = rc.LastError
	return Put(BucketReverseConfs, []byte(rc.Name), existing)
}

func GetReverseConfig(name string) (*config.ReverseConfig, error) {
	return getView[config.ReverseConfig](BucketReverseConfs, []byte(name))
}

func DeleteReverseConfig(name string) error {
	return Delete(BucketReverseConfs, []byte(name))
}

func ListReverseConfigs() ([]*config.ReverseConfig, error) {
	return listView[config.ReverseConfig](BucketReverseConfs)
}

// ===== 规则 =====

// ===== 新格式：每条规则独立存储 =====
// Key 结构：
// - "next_id" → uint64 (自增序列)
// - "order" → []uint64 (规则 ID 有序列表)
// - "rule:{id}" → string (规则内容)
var (
	KeyNextID    = []byte("next_id")
	KeyOrder     = []byte("order")
	RuleKeyPrefix = []byte("rule:")
)

// ruleKey 生成规则的存储 key: "rule:{id}"
func ruleKey(id uint64) []byte {
	return append(RuleKeyPrefix, []byte(fmt.Sprintf("%d", id))...)
}

// getNextIDTx 读取下一个可用 ID（事务内）
func getNextIDTx(tx *bolt.Tx) (uint64, error) {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return 0, fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	data := b.Get(KeyNextID)
	if data == nil {
		return 1, nil // 默认从 1 开始
	}
	var id uint64
	if err := json.Unmarshal(data, &id); err != nil {
		return 0, err
	}
	return id, nil
}

// putNextIDTx 设置下一个可用 ID（事务内）
func putNextIDTx(tx *bolt.Tx, id uint64) error {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	return putJSON(b, KeyNextID, id)
}

// getOrderTx 读取规则顺序列表（事务内）
func getOrderTx(tx *bolt.Tx) ([]uint64, error) {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return nil, fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	data := b.Get(KeyOrder)
	if data == nil {
		return []uint64{}, nil
	}
	var order []uint64
	if err := json.Unmarshal(data, &order); err != nil {
		return nil, err
	}
	return order, nil
}

// putOrderTx 设置规则顺序列表（事务内）
func putOrderTx(tx *bolt.Tx, order []uint64) error {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	return putJSON(b, KeyOrder, order)
}

// getRuleByIDTx 读取单条规则（事务内）
func getRuleByIDTx(tx *bolt.Tx, id uint64) (string, error) {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return "", fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	data := b.Get(ruleKey(id))
	if data == nil {
		return "", ErrNotFound
	}
	var rule string
	if err := json.Unmarshal(data, &rule); err != nil {
		return "", err
	}
	return rule, nil
}

// putRuleByIDTx 写入单条规则（事务内）
func putRuleByIDTx(tx *bolt.Tx, id uint64, rule string) error {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	return putJSON(b, ruleKey(id), rule)
}

// deleteRuleByIDTx 删除单条规则（事务内）
func deleteRuleByIDTx(tx *bolt.Tx, id uint64) error {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	return b.Delete(ruleKey(id))
}

// ===== 高层 API =====

// RuleWithID 表示带 ID 的规则
type RuleWithID struct {
	ID   uint64 `json:"id"`
	Rule string `json:"rule"`
}

// ListRules 返回所有规则（不带 ID，向后兼容）
func ListRules() ([]string, error) {
	rulesWithIDs, err := ListRulesWithIDs()
	if err != nil {
		return nil, err
	}
	rules := make([]string, len(rulesWithIDs))
	for i, r := range rulesWithIDs {
		rules[i] = r.Rule
	}
	return rules, nil
}

// ListRulesWithIDs 返回所有规则（带 ID）
func ListRulesWithIDs() ([]RuleWithID, error) {
	var rules []RuleWithID
	err := View(func(tx *bolt.Tx) error {
		order, err := getOrderTx(tx)
		if err != nil {
			return err
		}
		for _, id := range order {
			rule, err := getRuleByIDTx(tx, id)
			if err != nil {
				if err == ErrNotFound {
					continue // 跳过不存在的规则（数据不一致）
				}
				return err
			}
			rules = append(rules, RuleWithID{ID: id, Rule: rule})
		}
		return nil
	})
	return rules, err
}

// InsertRuleWithID 插入规则并返回新 ID
// afterID: 插入到该 ID 之后；nil=追加到末尾；如果 afterID 不存在则报错
func InsertRuleWithID(rule string, afterID *uint64) (uint64, error) {
	var newID uint64
	err := Update(func(tx *bolt.Tx) error {
		// 1. 分配新 ID
		nextID, err := getNextIDTx(tx)
		if err != nil {
			return err
		}
		newID = nextID
		
		// 2. 写入规则
		if err := putRuleByIDTx(tx, newID, rule); err != nil {
			return err
		}
		
		// 3. 更新顺序列表
		order, err := getOrderTx(tx)
		if err != nil {
			return err
		}
		
		if afterID == nil {
			// 追加到末尾
			order = append(order, newID)
		} else if *afterID == 0 {
			// afterID=0 表示插入到开头
			order = append([]uint64{newID}, order...)
		} else {
			// 插入到 afterID 之后
			pos := -1
			for i, id := range order {
				if id == *afterID {
					pos = i
					break
				}
			}
			if pos == -1 {
				return fmt.Errorf("after_id %d not found", *afterID)
			}
			// 在 pos+1 位置插入
			order = append(order[:pos+1], append([]uint64{newID}, order[pos+1:]...)...)
		}
		
		if err := putOrderTx(tx, order); err != nil {
			return err
		}
		
		// 4. 递增 next_id
		return putNextIDTx(tx, newID+1)
	})
	if err != nil {
		return 0, err
	}
	notifyChanges([]ChangeEvent{{
		Bucket: string(BucketRules), Key: KeyRebuilt, Op: OpPut,
	}})
	return newID, nil
}

// UpdateRuleByID 更新单条规则
func UpdateRuleByID(id uint64, rule string) error {
	err := Update(func(tx *bolt.Tx) error {
		// 检查规则是否存在
		_, err := getRuleByIDTx(tx, id)
		if err != nil {
			return err
		}
		return putRuleByIDTx(tx, id, rule)
	})
	if err != nil {
		return err
	}
	notifyChanges([]ChangeEvent{{
		Bucket: string(BucketRules), Key: KeyRebuilt, Op: OpPut,
	}})
	return nil
}

// DeleteRuleByID 删除单条规则
func DeleteRuleByID(id uint64) error {
	err := Update(func(tx *bolt.Tx) error {
		// 检查规则是否存在
		_, err := getRuleByIDTx(tx, id)
		if err != nil {
			return err
		}
		
		// 删除规则
		if err := deleteRuleByIDTx(tx, id); err != nil {
			return err
		}
		
		// 从 order 列表中移除
		order, err := getOrderTx(tx)
		if err != nil {
			return err
		}
		newOrder := make([]uint64, 0, len(order)-1)
		for _, oid := range order {
			if oid != id {
				newOrder = append(newOrder, oid)
			}
		}
		return putOrderTx(tx, newOrder)
	})
	if err != nil {
		return err
	}
	notifyChanges([]ChangeEvent{{
		Bucket: string(BucketRules), Key: KeyRebuilt, Op: OpPut,
	}})
	return nil
}

// ReorderRules 批量调整规则顺序
func ReorderRules(order []uint64) error {
	err := Update(func(tx *bolt.Tx) error {
		// 验证所有 ID 都存在
		for _, id := range order {
			if _, err := getRuleByIDTx(tx, id); err != nil {
				return fmt.Errorf("id %d not found: %w", id, err)
			}
		}
		return putOrderTx(tx, order)
	})
	if err != nil {
		return err
	}
	notifyChanges([]ChangeEvent{{
		Bucket: string(BucketRules), Key: KeyRebuilt, Op: OpPut,
	}})
	return nil
}

// listRulesTx 在事务内读取所有规则（供 matcher 重建使用）
func listRulesTx(tx *bolt.Tx) ([]string, error) {
	order, err := getOrderTx(tx)
	if err != nil {
		// Fresh database with no rules bucket yet: treat as empty rule set.
		if tx.Bucket(BucketRules) == nil {
			return nil, nil
		}
		return nil, err
	}
	rules := make([]string, 0, len(order))
	for _, id := range order {
		rule, err := getRuleByIDTx(tx, id)
		if err != nil {
			if err == ErrNotFound {
				continue // 跳过不存在的规则
			}
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// listRules 读取所有规则（供 Watch 使用）
func listRules() ([]string, error) {
	var rules []string
	err := View(func(tx *bolt.Tx) error {
		var err error
		rules, err = listRulesTx(tx)
		return err
	})
	return rules, err
}

// readScalarInto 读取标量配置节。key 不存在返回 (false, nil) 且不改 dest。
func readScalarInto(cb *bolt.Bucket, key []byte, dest interface{}) (bool, error) {
	data := cb.Get(key)
	if data == nil {
		return false, nil
	}
	if err := json.Unmarshal(data, dest); err != nil {
		return true, fmt.Errorf("解析 %s 失败: %w", key, err)
	}
	return true, nil
}

// ResetConfigPreserving has been removed. Use ReplaceAllConfig with preserved values instead.
// See admin/admin.go config reset handler for example usage.

// IsEmptyConfig 报告配置 bucket 是否全部为空。数据库文件存在但从未写入
// 配置时（如初始化中断），主流程仍需走导入流程。
func IsEmptyConfig() (bool, error) {
	empty := true
	err := View(func(tx *bolt.Tx) error {
		for _, bucket := range configBuckets() {
			b := tx.Bucket(bucket)
			if b == nil {
				continue
			}
			if b.Stats().KeyN > 0 {
				empty = false
				return nil
			}
		}
		return nil
	})
	return empty, err
}


