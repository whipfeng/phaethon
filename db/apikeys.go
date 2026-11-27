package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"
)

// 控制台 API 密钥。只保存 token 的 SHA-256（bucket key = ID），
// 明文仅在创建时返回一次，之后无法找回。

// APIKey 一条 API 密钥记录。
type APIKey struct {
	ID         string     `json:"id"` // SHA-256(token) 十六进制
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"` // 明文前缀，便于人工识别
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// HashAPIKey 返回 token 的 SHA-256 十六进制摘要。
func HashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Expired 报告密钥在 now 时刻是否已过期。
func (k *APIKey) Expired(now time.Time) bool {
	return k != nil && k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}

// ListAPIKeys 列出全部密钥，按创建时间升序。
func ListAPIKeys() ([]APIKey, error) {
	keys := []APIKey{}
	err := ForEach(BucketAPIKeys, func(k, v []byte) error {
		var key APIKey
		if err := json.Unmarshal(v, &key); err != nil {
			return err
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].CreatedAt.Before(keys[j].CreatedAt)
	})
	return keys, nil
}

// GetAPIKey 按 ID 取单条密钥。
func GetAPIKey(id string) (*APIKey, error) {
	var key APIKey
	if err := Get(BucketAPIKeys, []byte(id), &key); err != nil {
		return nil, err
	}
	return &key, nil
}

// FindAPIKeyBySecret 按明文 token 反查密钥记录。
func FindAPIKeyBySecret(secret string) (*APIKey, error) {
	return GetAPIKey(HashAPIKey(secret))
}

// PutAPIKey 写入密钥记录（同 ID 覆盖）。
func PutAPIKey(key *APIKey) error {
	return Put(BucketAPIKeys, []byte(key.ID), key)
}

// DeleteAPIKey 删除密钥。
func DeleteAPIKey(id string) error {
	return Delete(BucketAPIKeys, []byte(id))
}

// TouchAPIKey 更新最后使用时间。调用方负责限频，避免每次请求都写库。
func TouchAPIKey(id string, now time.Time) error {
	key, err := GetAPIKey(id)
	if err != nil {
		return err
	}
	key.LastUsedAt = &now
	return PutAPIKey(key)
}
