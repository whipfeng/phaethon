package db

import (
	"encoding/binary"

	bolt "go.etcd.io/bbolt"
)

// incrementVersion 递增指定 topic 的版本号（事务内原子操作）
func incrementVersion(topic string) (uint64, error) {
	var version uint64
	err := globalDB.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(BucketVersions)
		if bucket == nil {
			return nil
		}

		// 读
		if data := bucket.Get([]byte(topic)); data != nil && len(data) == 8 {
			version = binary.LittleEndian.Uint64(data)
		}

		// 写
		version++
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, version)
		return bucket.Put([]byte(topic), buf)
	})
	return version, err
}

// readVersion 读取指定 topic 的版本号
func readVersion(topic string) uint64 {
	var version uint64
	_ = globalDB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(BucketVersions)
		if bucket == nil {
			return nil
		}
		if data := bucket.Get([]byte(topic)); data != nil && len(data) == 8 {
			version = binary.LittleEndian.Uint64(data)
		}
		return nil
	})
	return version
}

// readAllVersions 读取所有版本号
func readAllVersions() map[string]uint64 {
	result := make(map[string]uint64)
	_ = globalDB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(BucketVersions)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			if len(v) == 8 {
				result[string(k)] = binary.LittleEndian.Uint64(v)
			}
			return nil
		})
	})
	return result
}
