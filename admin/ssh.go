package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"phaethon/db"
	"phaethon/util"
)

// apiSSH handles GET /api/ssh — SSH 服务总览：开关、host key 指纹、公钥列表。
func (s *AdminServer) apiSSH(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg, err := db.GetSSHConfig()
	if err != nil {
		httpError(w, "load config fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	keys, err := db.ListSSHKeys()
	if err != nil {
		httpError(w, "load keys fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if keys == nil {
		keys = []db.SSHKey{}
	}

	hostFP := ""
	if pemBytes, err := db.GetSSHHostKey(); err == nil {
		if signer, err := ssh.ParsePrivateKey(pemBytes); err == nil {
			hostFP = ssh.FingerprintSHA256(signer.PublicKey())
		}
	}

	jsonResponse(w, map[string]interface{}{
		"enabled":             cfg.Enabled,
		"hostKeyFingerprint":  hostFP,
		"keys":                keys,
	})
}

// apiSSHConfig handles PUT /api/ssh/config — 启停开关。
func (s *AdminServer) apiSSHConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
		return
	}
	cfg := &db.SSHConfig{Enabled: req.Enabled}
	if err := db.PutSSHConfig(cfg); err != nil {
		httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	util.LogModuleInfo("admin", "ssh config updated: enabled=%v", req.Enabled)
	jsonResponse(w, map[string]interface{}{"ok": true})
}

// apiSSHKeys handles GET/POST /api/ssh/keys — 公钥列表 / 新增公钥。
func (s *AdminServer) apiSSHKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		keys, err := db.ListSSHKeys()
		if err != nil {
			httpError(w, "load fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if keys == nil {
			keys = []db.SSHKey{}
		}
		jsonResponse(w, keys)

	case http.MethodPost:
		var req struct {
			Name   string `json:"name"`
			PubKey string `json:"pubkey"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, "parse fail: "+err.Error(), http.StatusBadRequest)
			return
		}
		pubKey := strings.TrimSpace(req.PubKey)
		if pubKey == "" {
			httpError(w, "pubkey required", http.StatusBadRequest)
			return
		}
		parsed, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(pubKey))
		if err != nil {
			httpError(w, "invalid public key: "+err.Error(), http.StatusBadRequest)
			return
		}
		fp := ssh.FingerprintSHA256(parsed)
		if _, err := db.GetSSHKey(fp); err == nil {
			httpError(w, "key already exists", http.StatusConflict)
			return
		}

		name := strings.TrimSpace(req.Name)
		if name == "" {
			name = comment
		}
		key := &db.SSHKey{
			Fingerprint: fp,
			Name:        name,
			PubKey:      pubKey,
			CreatedAt:   time.Now(),
		}
		if err := db.PutSSHKey(key); err != nil {
			httpError(w, "save fail: "+err.Error(), http.StatusInternalServerError)
			return
		}
		util.LogModuleInfo("admin", "ssh key added: name=%s fp=%s", name, fp)
		jsonResponse(w, key)

	default:
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// apiSSHKeyItem handles DELETE /api/ssh/keys/{fingerprint} — 删除公钥。
// 指纹含 base64 特殊字符，前端须 encodeURIComponent。
func (s *AdminServer) apiSSHKeyItem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		httpError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fp := strings.TrimPrefix(r.URL.Path, "/api/ssh/keys/")
	fp, err := url.PathUnescape(fp)
	if err != nil || fp == "" {
		httpError(w, "invalid fingerprint", http.StatusBadRequest)
		return
	}
	if err := db.DeleteSSHKey(fp); err != nil {
		httpError(w, "delete fail: "+err.Error(), http.StatusInternalServerError)
		return
	}
	util.LogModuleInfo("admin", "ssh key deleted: fp=%s", fp)
	jsonResponse(w, map[string]interface{}{"ok": true})
}
