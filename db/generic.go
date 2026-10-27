package db

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// ListBucketKeys returns all keys in a bucket
func ListBucketKeys(bucket string) ([]string, error) {
	var keys []string
	err := View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %s not found", bucket)
		}
		
		return b.ForEach(func(k, v []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	})
	
	return keys, err
}

// GetRawKeyValue returns the raw value for a key in a bucket
func GetRawKeyValue(bucket, key string) ([]byte, error) {
	var value []byte
	err := View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %s not found", bucket)
		}
		
		v := b.Get([]byte(key))
		if v == nil {
			return ErrNotFound
		}
		
		value = make([]byte, len(v))
		copy(value, v)
		return nil
	})
	
	return value, err
}

// PutRawKeyValue sets a raw key-value pair in a bucket
func PutRawKeyValue(bucket, key string, value []byte) error {
	return Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), value)
	})
}

// DeleteKeyValue deletes a key from a bucket
func DeleteKeyValue(bucket, key string) error {
	return Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("bucket %s not found", bucket)
		}
		return b.Delete([]byte(key))
	})
}

// GetJSONKeyValue gets a key and unmarshals it as JSON
func GetJSONKeyValue(bucket, key string, v interface{}) error {
	data, err := GetRawKeyValue(bucket, key)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// PutJSONKeyValue marshals v as JSON and stores it
func PutJSONKeyValue(bucket, key string, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return PutRawKeyValue(bucket, key, data)
}
