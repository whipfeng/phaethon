package db

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"phaethon/config"
)

// BucketConfig 内的标量配置 key
var (
	keyAdmin        = []byte("admin")
	keyTUN          = []byte("tun")
	keyMesh         = []byte("mesh")
	keyInteractive  = []byte("interactive")
	keyUDPPortRange = []byte("udp-port-range")
	keyTCPPortRange = []byte("tcp-port-range")
)

// KeyRebuilt 是 ImportRuleConf 提交后发出的合成事件 key，
// 重建 watcher 收到即全量 LoadRuleConf。
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
	return Put(BucketConfig, keyAdmin, c)
}

func GetAdminConfig() (*config.AdminConfig, error) {
	return getView[config.AdminConfig](BucketConfig, keyAdmin)
}

func PutTUNConfig(c *config.TUNConfig) error {
	return Put(BucketConfig, keyTUN, c)
}

func GetTUNConfig() (*config.TUNConfig, error) {
	return getView[config.TUNConfig](BucketConfig, keyTUN)
}

func PutMeshConfig(c *config.MeshConfig) error {
	return Put(BucketConfig, keyMesh, c)
}

func GetMeshConfig() (*config.MeshConfig, error) {
	return getView[config.MeshConfig](BucketConfig, keyMesh)
}

func PutInteractive(v bool) error {
	return Put(BucketConfig, keyInteractive, v)
}

func GetInteractive() (bool, error) {
	v, err := getView[bool](BucketConfig, keyInteractive)
	if err != nil {
		return false, err
	}
	return *v, nil
}

func PutUDPPortRange(s string) error {
	return Put(BucketConfig, keyUDPPortRange, s)
}

func GetUDPPortRange() (string, error) {
	v, err := getView[string](BucketConfig, keyUDPPortRange)
	if err != nil {
		return "", err
	}
	return *v, nil
}

func PutTCPPortRange(s string) error {
	return Put(BucketConfig, keyTCPPortRange, s)
}

func GetTCPPortRange() (string, error) {
	v, err := getView[string](BucketConfig, keyTCPPortRange)
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

// storeGroup 返回可持久化的代理组副本，清零运行时派生与废弃字段。
// 不能整体拷贝结构体：内含 sync.RWMutex（go vet copylocks 会报错）。
func storeGroup(g *config.ProxyGroup) *config.ProxyGroup {
	// Merge Proxies and ManualProxies for backwards compatibility
	allProxies := append([]string(nil), g.ManualProxies...)
	for _, p := range g.Proxies {
		found := false
		for _, mp := range g.ManualProxies {
			if mp == p {
				found = true
				break
			}
		}
		if !found {
			allProxies = append(allProxies, p)
		}
	}
	
	return &config.ProxyGroup{
		Name:                 g.Name,
		Enabled:              g.Enabled,
		Type:                 g.Type,
		ManualProxies:        allProxies,
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
	return listView[config.ProxyGroup](BucketProxyGroups)
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

// ruleKey 将规则索引编码为零填充 key，保证 bucket 遍历即规则顺序。
func ruleKey(index int) []byte {
	return []byte(fmt.Sprintf("%06d", index))
}

func PutRule(index int, rule string) error {
	return Put(BucketRules, ruleKey(index), rule)
}

func DeleteRule(index int) error {
	return Delete(BucketRules, ruleKey(index))
}

func listRulesTx(tx *bolt.Tx) ([]string, error) {
	b := tx.Bucket(BucketRules)
	if b == nil {
		return nil, fmt.Errorf("bucket %s 不存在", string(BucketRules))
	}
	rules := []string{}
	err := b.ForEach(func(k, v []byte) error {
		var r string
		if err := json.Unmarshal(v, &r); err != nil {
			return fmt.Errorf("解析规则 %s 失败: %w", k, err)
		}
		rules = append(rules, r)
		return nil
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

// ===== 整份配置装配 =====

// LoadRuleConf 从数据库全量装配 *config.RuleConfiguration 并执行 Init()。
// 所有 bucket 在同一个只读事务中读取，保证快照一致。
// 缺失的节保持零值（Admin/TUN/Mesh 为 nil，列表为空）。
func LoadRuleConf() (*config.RuleConfiguration, error) {
	conf := &config.RuleConfiguration{}
	err := View(func(tx *bolt.Tx) error {
		var err error
		if conf.Proxies, err = listTx[config.Proxy](tx, BucketProxies); err != nil {
			return err
		}
		if conf.ProxyGroups, err = listTx[config.ProxyGroup](tx, BucketProxyGroups); err != nil {
			return err
		}
		if conf.Subscriptions, err = listTx[config.Subscription](tx, BucketSubscriptions); err != nil {
			return err
		}
		if conf.Rules, err = listRulesTx(tx); err != nil {
			return err
		}
		if conf.Mappings, err = listTx[config.Mapping](tx, BucketMappings); err != nil {
			return err
		}
		if conf.Resolvers, err = listTx[config.Resolver](tx, BucketResolvers); err != nil {
			return err
		}
		if conf.ReverseConfigs, err = listTx[config.ReverseConfig](tx, BucketReverseConfs); err != nil {
			return err
		}

		cb := tx.Bucket(BucketConfig)

		var admin config.AdminConfig
		if present, err := readScalarInto(cb, keyAdmin, &admin); err != nil {
			return err
		} else if present {
			conf.Admin = &admin
		}

		var tun config.TUNConfig
		if present, err := readScalarInto(cb, keyTUN, &tun); err != nil {
			return err
		} else if present {
			conf.TUN = &tun
		}

		var mesh config.MeshConfig
		if present, err := readScalarInto(cb, keyMesh, &mesh); err != nil {
			return err
		} else if present {
			conf.Mesh = &mesh
		}

		if _, err := readScalarInto(cb, keyInteractive, &conf.Interactive); err != nil {
			return err
		}
		if _, err := readScalarInto(cb, keyUDPPortRange, &conf.UDPPortRange); err != nil {
			return err
		}
		if _, err := readScalarInto(cb, keyTCPPortRange, &conf.TCPPortRange); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := conf.Init(); err != nil {
		return nil, fmt.Errorf("config init: %w", err)
	}
	return conf, nil
}

// SyncConfigToDB 将内存中的配置精确同步到数据库：单个写事务内对每个 bucket 做 key 级
// 精确对比（删除 conf 中不存在的旧 key，写入/更新 conf 中存在的 key），不触碰 fakeip/packages。
// 提交成功后触发一次合成变更事件（config/__rebuilt__），供重建 watcher 全量重建快照。
//
// 与旧 ImportRuleConf 的区别：
// 1. 不再"先 DeleteBucket 再 CreateBucket"，而是逐个 key 精确对比，避免事务中途失败导致数据丢失
// 2. 语义从"从配置文件导入"改为"同步内存配置到数据库"
// 3. 安全检查：如果 conf 为 nil 或所有列表项均为空，拒绝执行，防止意外清空数据库
func SyncConfigToDB(conf *config.RuleConfiguration) error {
	// 验证配置完整性，防止意外清空数据库
	if conf == nil {
		return fmt.Errorf("拒绝同步空配置：conf 为 nil")
	}
	
	// 检查是否所有列表都为空（可能是未初始化的配置）
	allListsEmpty := len(conf.Proxies) == 0 &&
		len(conf.ProxyGroups) == 0 &&
		len(conf.Subscriptions) == 0 &&
		len(conf.Rules) == 0 &&
		len(conf.Mappings) == 0 &&
		len(conf.Resolvers) == 0 &&
		len(conf.ReverseConfigs) == 0
	
	if allListsEmpty {
		return fmt.Errorf("拒绝同步空配置：所有列表项（Proxies/Rules/Mappings 等）均为空，可能是未初始化的配置")
	}
	
	err := Update(func(tx *bolt.Tx) error {
		cb := tx.Bucket(BucketConfig)
		pb := tx.Bucket(BucketProxies)
		gb := tx.Bucket(BucketProxyGroups)
		sb := tx.Bucket(BucketSubscriptions)
		rb := tx.Bucket(BucketRules)
		mb := tx.Bucket(BucketMappings)
		resb := tx.Bucket(BucketResolvers)
		rvb := tx.Bucket(BucketReverseConfs)

		// 同步列表类型：对比现有 keys 和 conf 中的 keys，删除多余的，写入新增的
		if err := syncNamedList(pb, conf.Proxies, func(p *config.Proxy) (string, interface{}) {
			return p.Name, p
		}); err != nil {
			return fmt.Errorf("同步 proxies 失败: %w", err)
		}
		if err := syncNamedList(gb, conf.ProxyGroups, func(g *config.ProxyGroup) (string, interface{}) {
			return g.Name, storeGroup(g)
		}); err != nil {
			return fmt.Errorf("同步 proxy_groups 失败: %w", err)
		}
		if err := syncNamedList(sb, conf.Subscriptions, func(s *config.Subscription) (string, interface{}) {
			return s.Name, s
		}); err != nil {
			return fmt.Errorf("同步 subscriptions 失败: %w", err)
		}
		if err := syncNamedList(mb, conf.Mappings, func(m *config.Mapping) (string, interface{}) {
			return m.Name, m
		}); err != nil {
			return fmt.Errorf("同步 mappings 失败: %w", err)
		}
		if err := syncNamedList(resb, conf.Resolvers, func(r *config.Resolver) (string, interface{}) {
			return r.Name, r
		}); err != nil {
			return fmt.Errorf("同步 resolvers 失败: %w", err)
		}
		if err := syncNamedList(rvb, conf.ReverseConfigs, func(rc *config.ReverseConfig) (string, interface{}) {
			return rc.Name, storeReverse(rc)
		}); err != nil {
			return fmt.Errorf("同步 reverse_configs 失败: %w", err)
		}
		
		// Rules 特殊处理：用索引作为 key
		if err := syncRules(rb, conf.Rules); err != nil {
			return fmt.Errorf("同步 rules 失败: %w", err)
		}

		// 同步标量配置：直接覆盖写
		if conf.Admin != nil {
			if err := putJSON(cb, keyAdmin, conf.Admin); err != nil {
				return err
			}
		}
		if conf.TUN != nil {
			if err := putJSON(cb, keyTUN, conf.TUN); err != nil {
				return err
			}
		}
		if conf.Mesh != nil {
			if err := putJSON(cb, keyMesh, conf.Mesh); err != nil {
				return err
			}
		}
		if err := putJSON(cb, keyInteractive, conf.Interactive); err != nil {
			return err
		}
		if conf.UDPPortRange != "" {
			if err := putJSON(cb, keyUDPPortRange, conf.UDPPortRange); err != nil {
				return err
			}
		} else {
			// 如果为空，删除旧 key
			_ = cb.Delete(keyUDPPortRange)
		}
		if conf.TCPPortRange != "" {
			if err := putJSON(cb, keyTCPPortRange, conf.TCPPortRange); err != nil {
				return err
			}
		} else {
			// 如果为空，删除旧 key
			_ = cb.Delete(keyTCPPortRange)
		}
		return nil
	})
	if err != nil {
		return err
	}

	notifyChanges([]ChangeEvent{{
		Bucket: string(BucketConfig), Key: KeyRebuilt, Op: OpPut,
	}})
	return nil
}

// syncNamedList 同步命名列表到 bucket：删除 conf 中不存在的旧 key，写入新增/修改的 key。
// keyFunc 从项中提取 name 和可序列化的值。
func syncNamedList[T any](b *bolt.Bucket, items []*T, keyFunc func(*T) (string, interface{})) error {
	// 收集 conf 中应该存在的 keys
	newKeys := make(map[string]bool)
	for _, item := range items {
		if item == nil {
			continue
		}
		name, value := keyFunc(item)
		newKeys[name] = true
		if err := putJSON(b, []byte(name), value); err != nil {
			return err
		}
	}
	
	// 删除 bucket 中存在但 conf 中不存在的旧 keys
	return b.ForEach(func(k, v []byte) error {
		key := string(k)
		if !newKeys[key] {
			return b.Delete(k)
		}
		return nil
	})
}

// syncRules 同步规则列表到 bucket：用索引作为 key，删除多余的旧规则。
func syncRules(b *bolt.Bucket, rules []string) error {
	// 写入新规则
	for i, r := range rules {
		if err := putJSON(b, ruleKey(i), r); err != nil {
			return err
		}
	}
	
	// 删除多余的旧规则（索引 >= len(rules) 的）
	newLen := len(rules)
	return b.ForEach(func(k, v []byte) error {
		// 解析索引
		var idx int
		if _, err := fmt.Sscanf(string(k), "%d", &idx); err != nil {
			return nil // 忽略无法解析的 key
		}
		if idx >= newLen {
			return b.Delete(k)
		}
		return nil
	})
}

// MigrateProxyGroups updates existing proxy groups with proxies from the config.
// This fixes the issue where old migrations only stored ManualProxies but not Proxies.
func MigrateProxyGroups(conf *config.RuleConfiguration) error {
	return Update(func(tx *bolt.Tx) error {
		gb := tx.Bucket(BucketProxyGroups)
		if gb == nil {
			return nil
		}

		for _, g := range conf.ProxyGroups {
			if g == nil {
				continue
			}
			// Read existing group from DB
			data := gb.Get([]byte(g.Name))
			if data == nil {
				continue
			}
			var existing config.ProxyGroup
			if err := json.Unmarshal(data, &existing); err != nil {
				continue
			}

			// If ManualProxies is empty but config has Proxies, update it
			if len(existing.ManualProxies) == 0 && len(g.Proxies) > 0 {
				existing.ManualProxies = append([]string(nil), g.Proxies...)
				if err := putJSON(gb, []byte(g.Name), &existing); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ResetConfigPreserving has been removed. Use SyncConfigToDB with preserved values instead.
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
