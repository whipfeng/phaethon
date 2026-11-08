package db

import (
	"encoding/json"
	"time"
)

// SSH 服务相关存储。host key / enabled / 公钥均为本节点本地状态，
// 不参与跨节点配置版本同步（二期再议全局同步）。

var (
	BucketSSH     = []byte("ssh")      // 本节点 SSH 服务状态：host key、enabled
	BucketSSHKeys = []byte("ssh_keys") // authorized 公钥（key = SHA256 指纹）

	sshHostKeyItem = []byte("host_ed25519")
	sshConfigItem  = []byte("config")
)

// SSHConfig 本节点 SSH 服务配置。
// Shell 为空字符串时走各平台自动探测（Windows: pwsh→powershell→cmd；Unix: $SHELL→/bin/sh）。
type SSHConfig struct {
	Enabled bool   `json:"enabled"`
	Shell   string `json:"shell,omitempty"`
}

// SSHKey 一条 authorized 公钥。
type SSHKey struct {
	Fingerprint string    `json:"fingerprint"` // SHA256 指纹（bucket key）
	Name        string    `json:"name"`        // 备注（设备/归属）
	PubKey      string    `json:"pubkey"`      // authorized_keys 单行原文
	CreatedAt   time.Time `json:"created_at"`
}

// GetSSHConfig 读取 SSH 开关；未配置时默认启用（无钥匙时无人可登录，无风险）。
func GetSSHConfig() (*SSHConfig, error) {
	var cfg SSHConfig
	err := Get(BucketSSH, sshConfigItem, &cfg)
	if err == ErrNotFound {
		return &SSHConfig{Enabled: true}, nil
	}
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func PutSSHConfig(cfg *SSHConfig) error {
	return Put(BucketSSH, sshConfigItem, cfg)
}

// GetSSHHostKey 读取 PEM 编码的 SSH host private key。
func GetSSHHostKey() ([]byte, error) {
	var pemBytes []byte
	err := Get(BucketSSH, sshHostKeyItem, &pemBytes)
	return pemBytes, err
}

// PutSSHHostKey 保存 PEM 编码的 SSH host private key。
func PutSSHHostKey(pemBytes []byte) error {
	return Put(BucketSSH, sshHostKeyItem, pemBytes)
}

// ListSSHKeys 列出全部 authorized 公钥。
func ListSSHKeys() ([]SSHKey, error) {
	keys := []SSHKey{}
	err := ForEach(BucketSSHKeys, func(k, v []byte) error {
		var key SSHKey
		if err := json.Unmarshal(v, &key); err != nil {
			return err
		}
		keys = append(keys, key)
		return nil
	})
	return keys, err
}

// GetSSHKey 按指纹取单条公钥。
func GetSSHKey(fingerprint string) (*SSHKey, error) {
	var key SSHKey
	err := Get(BucketSSHKeys, []byte(fingerprint), &key)
	if err != nil {
		return nil, err
	}
	return &key, nil
}

// PutSSHKey 新增公钥（fingerprint 已存在则覆盖）。
func PutSSHKey(key *SSHKey) error {
	return Put(BucketSSHKeys, []byte(key.Fingerprint), key)
}

// DeleteSSHKey 删除公钥。
func DeleteSSHKey(fingerprint string) error {
	return Delete(BucketSSHKeys, []byte(fingerprint))
}
