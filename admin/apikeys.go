package admin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"phaethon/db"
	"phaethon/util"
)

const apiKeyPrefix = "phk_"

func newAPIKeySecret() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	secret := apiKeyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	prefix := secret
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return secret, prefix, nil
}

// apiAPIKeys handles GET/POST /api/api-keys — 列出 / 新建 API 密钥。
// 明文仅在 POST 响应里返回一次，列表永远不含明文和哈希以外的敏感信息。
func (s *AdminServer) apiAPIKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys, err := db.ListAPIKeys()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, keys)

	case http.MethodPost:
		var req struct {
			Name          string `json:"name"`
			ExpiresInDays int    `json:"expires_in_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			httpError(w, "name required", http.StatusBadRequest)
			return
		}
		if len(name) > 64 {
			httpError(w, "name too long", http.StatusBadRequest)
			return
		}
		if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
			httpError(w, "expires_in_days out of range", http.StatusBadRequest)
			return
		}

		secret, prefix, err := newAPIKeySecret()
		if err != nil {
			httpError(w, "generate key fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()
		key := &db.APIKey{
			ID:        db.HashAPIKey(secret),
			Name:      name,
			Prefix:    prefix,
			CreatedAt: now,
		}
		if req.ExpiresInDays > 0 {
			exp := now.Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
			key.ExpiresAt = &exp
		}
		if err := db.PutAPIKey(key); err != nil {
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.LogModuleInfo("admin", "api key created: name=%s prefix=%s expires_in_days=%d", name, prefix, req.ExpiresInDays)
		jsonResponse(w, map[string]interface{}{"key": secret, "record": key})

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiAPIKeyItem handles DELETE /api/api-keys/{id} — 删除密钥。
func (s *AdminServer) apiAPIKeyItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/api-keys/"))
	if err != nil || id == "" {
		httpError(w, "invalid id", http.StatusBadRequest)
		return
	}
	key, err := db.GetAPIKey(id)
	if err != nil {
		httpError(w, "not found", http.StatusNotFound)
		return
	}
	if err := db.DeleteAPIKey(id); err != nil {
		httpError(w, "delete fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	util.LogModuleInfo("admin", "api key deleted: name=%s prefix=%s", key.Name, key.Prefix)
	jsonResponse(w, map[string]interface{}{"ok": true})
}
