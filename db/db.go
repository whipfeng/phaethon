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
	BucketConfig        = []byte("config")
	BucketProxies       = []byte("proxies")
	BucketProxyGroups   = []byte("proxy-groups")
	BucketSubscriptions = []byte("subscriptions")
	BucketRules         = []byte("rules")
	BucketMappings      = []byte("mappings")
	BucketResolvers     = []byte("resolvers")
	BucketReverseConfs  = []byte("reverse-configs")
	BucketFakeIP        = []byte("fakeip")
	BucketPackages      = []byte("packages")
)

// ErrNotFound 表示 key 不存在
var ErrNotFound = errors.New("key 不存在")

// configBuckets 返回所有承载配置数据的 bucket（批量导入/重置时整体替换）。
// fakeip/packages 是状态与历史数据，不属于配置，不在其中。
func configBuckets() [][]byte {
	return [][]byte{
		BucketConfig,
		BucketProxies,
		BucketProxyGroups,
		BucketSubscriptions,
		BucketRules,
		BucketMappings,
		BucketResolvers,
		BucketReverseConfs,
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
		for _, bucket := range [][]byte{BucketFakeIP, BucketPackages} {
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
// 批量写入方应在提交成功后自行通知（见 configstore.ImportRuleConf）。
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
