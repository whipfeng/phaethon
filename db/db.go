package db

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	// 全局数据库实例
	globalDB *bolt.DB
	
	// Bucket 名称
	BucketConfig    = []byte("config")
	BucketProxies   = []byte("proxies")
	BucketRules     = []byte("rules")
	BucketFakeIP    = []byte("fakeip")
	BucketPackages  = []byte("packages")
)

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
		buckets := [][]byte{
			BucketConfig,
			BucketProxies,
			BucketRules,
			BucketFakeIP,
			BucketPackages,
		}
		
		for _, bucket := range buckets {
			_, err := tx.CreateBucketIfNotExists(bucket)
			if err != nil {
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
	return globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %s 不存在", string(bucket))
		}
		
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("序列化失败: %w", err)
		}
		
		return b.Put(key, data)
	})
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
			return fmt.Errorf("key 不存在")
		}
		
		return json.Unmarshal(data, dest)
	})
}

// Delete 删除键值对
func Delete(bucket, key []byte) error {
	return globalDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		if b == nil {
			return fmt.Errorf("bucket %s 不存在", string(bucket))
		}
		
		return b.Delete(key)
	})
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
