package db

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	// 全局数据库实例
	globalDB *bolt.DB

	// Bucket 名称
	// 配置类 bucket
	BucketConfig            = []byte("config")              // 旧：统一配置（待迁移）
	BucketAdminConfig       = []byte("admin-config")        // 新：管理配置
	BucketTUNConfig         = []byte("tun-config")          // 新：TUN 配置
	BucketMeshConfig        = []byte("mesh-config")         // 新：Mesh 网络配置
	BucketInteractiveConfig = []byte("interactive-config")  // 新：交互式配置
	BucketPortRangeConfig   = []byte("port-range-config")   // 新：端口范围配置
	BucketProxies           = []byte("proxies")             // 代理配置
	BucketProxyGroups       = []byte("proxy-groups")        // 代理组
	BucketSubscriptions     = []byte("subscriptions")       // 订阅配置
	BucketSubscriptionNodes = []byte("subscription-nodes")  // 订阅节点池
	BucketRules             = []byte("rules")               // 规则列表
	BucketMappings          = []byte("mappings")            // 映射配置
	BucketResolvers         = []byte("resolvers")           // DNS 解析器
	BucketReverseConfs      = []byte("reverse-configs")     // 反向连接配置
	BucketPackages          = []byte("packages")            // 套餐配置
	BucketCertificates      = []byte("certificates")        // TLS 证书
	BucketAPIKeys           = []byte("api-keys")            // API 密钥
	BucketWebhooks          = []byte("webhooks")            // Webhook 配置
	BucketLogs              = []byte("logs")                // 日志配置

	// 身份与状态 bucket
	BucketState             = []byte("state")               // 旧：统一状态（待迁移）
	BucketInstanceIdentity  = []byte("instance-identity")   // 新：实例身份
	BucketRuntimeState      = []byte("runtime-state")       // 新：运行时状态

	// 运行时状态 bucket（不触发版本通知）
	BucketFakeIP            = []byte("fakeip")              // Fake-IP 映射
	BucketProxyHealth       = []byte("proxy-health")        // 代理健康检查
	BucketAuditLog          = []byte("audit-log")           // 审计日志

	// 版本号管理
	BucketVersions          = []byte("versions")            // 统一版本号存储
)

// ErrNotFound 表示 key 不存在
var ErrNotFound = errors.New("key 不存在")

// configBuckets 返回所有承载配置数据的 bucket。
// fakeip/proxy-health/audit-log 是运行时状态，不属于配置，不在其中。
func configBuckets() [][]byte {
	return [][]byte{
		// 旧 bucket（迁移期间保留）
		BucketConfig,
		// 新 bucket（拆分后）
		BucketAdminConfig,
		BucketTUNConfig,
		BucketMeshConfig,
		BucketInteractiveConfig,
		BucketPortRangeConfig,
		// 其他配置 bucket
		BucketProxies,
		BucketProxyGroups,
		BucketSubscriptions,
		BucketRules,
		BucketMappings,
		BucketResolvers,
		BucketReverseConfs,
		BucketPackages,
		BucketCertificates,
		BucketAPIKeys,
		BucketWebhooks,
		BucketLogs,
	}
}

// Init 初始化数据库
func Init(dbPath string) error {
	var err error
	
	// 检查数据库是否存在
	_, err = os.Stat(dbPath)
	dbExists = !os.IsNotExist(err)
	
	// 打开数据库
	globalDB, err = bolt.Open(dbPath, 0600, &bolt.Options{
		Timeout: 1 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	
	// 创建所有 bucket
	err = globalDB.Update(func(tx *bolt.Tx) error {
		for _, bucket := range configBuckets() {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("创建 bucket %s 失败: %w", string(bucket), err)
			}
		}
		for _, bucket := range [][]byte{BucketFakeIP, BucketPackages, BucketProxyHealth, BucketSubscriptionNodes, BucketInstanceIdentity, BucketRuntimeState, BucketVersions} {
			if _, err := tx.CreateBucketIfNotExists(bucket); err != nil {
				return fmt.Errorf("创建 bucket %s 失败: %w", string(bucket), err)
			}
		}
		return nil
	})
	
	if err != nil {
		globalDB.Close()
		return err
	}
	
	return nil
}

// Close 关闭数据库
func Close() error {
	if globalDB != nil {
		return globalDB.Close()
	}
	return nil
}

// IsInitialized 检查数据库是否已初始化
func IsInitialized() bool {
	return globalDB != nil
}

// IsFirstRun 检查是否首次运行
func IsFirstRun() bool {
	return !dbExists
}

var dbExists bool

// Put 存储键值对
func Put(bucket, key []byte, value interface{}) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}

	err = globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %s 不存在", string(bucket))
		}
		return b.Put(key, data)
	})
	if err != nil {
		return err
	}

	notifyChanges([]ChangeEvent{{Bucket: string(bucket), Key: string(key), Op: OpPut}})
	return nil
}

// Get 获取键值对
func Get(bucket, key []byte, dest interface{}) error {
	return globalDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %s 不存在", string(bucket))
		}
		
		data := b.Get(key)
		if data == nil {
			return ErrNotFound
		}

		return json.Unmarshal(data, dest)
	})
}

// View 在只读事务中执行 fn（跨 bucket 一致快照）。不触发变更事件。
func View(fn func(tx *bolt.Tx) error) error {
	return globalDB.View(fn)
}

// Update 在写事务中执行 fn。与 Put/Delete 不同，它不自动触发变更事件；
// 批量写入方应在提交成功后自行通知（见 configstore.ReplaceAllConfig）。
func Update(fn func(tx *bolt.Tx) error) error {
	return globalDB.Update(fn)
}

// Delete 删除键值对
func Delete(bucket, key []byte) error {
	err := globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %s 不存在", string(bucket))
		}
		return b.Delete(key)
	})
	if err != nil {
		return err
	}

	notifyChanges([]ChangeEvent{{Bucket: string(bucket), Key: string(key), Op: OpDelete}})
	return nil
}

// ForEach 遍历 bucket 中的所有键值对
func ForEach(bucket []byte, fn func(k, v []byte) error) error {
	return globalDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %s 不存在", string(bucket))
		}
		
		return b.ForEach(fn)
	})
}
