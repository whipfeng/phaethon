package db

import (
	"time"
)

// DHCPLease 存储单个 DHCP 租约
type DHCPLease struct {
	IP        string    `json:"ip"`
	MAC       string    `json:"mac"`
	Hostname  string    `json:"hostname,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// PortBinding 存储端口绑定（与 server.PortBinding 保持一致）
type PortBinding struct {
	ReverseID      string    `json:"reverseId"`
	Seq            int       `json:"seq"`
	Port           int       `json:"port"`
	ListenerProto  string    `json:"listenerProto"`
	Identity       string    `json:"identity"`
	DirectDstHost  string    `json:"directDstHost,omitempty"`
	DirectDstPort  int       `json:"directDstPort,omitempty"`
	RegistryProxy  string    `json:"registryProxy,omitempty"`
	ControlAddr    string    `json:"controlAddr"`
	RegistryAddr   string    `json:"registryAddr"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	DisconnectedAt time.Time `json:"disconnectedAt"`
}

// ========== 运行时状态 ==========

// GetSessionSecret 获取 admin 会话密钥
func GetSessionSecret() ([]byte, error) {
	secret, err := getView[[]byte](BucketRuntimeState, []byte("session-secret"))
	if err != nil {
		return nil, err
	}
	if secret == nil {
		return nil, nil
	}
	return *secret, nil
}

// PutSessionSecret 存储 admin 会话密钥
func PutSessionSecret(secret []byte) error {
	return Put(BucketRuntimeState, []byte("session-secret"), secret)
}

// GetTLSCertificate 获取自动生成的 TLS 证书
func GetTLSCertificate() ([]byte, error) {
	cert, err := getView[[]byte](BucketRuntimeState, []byte("tls-cert"))
	if err != nil {
		return nil, err
	}
	if cert == nil {
		return nil, nil
	}
	return *cert, nil
}

// PutTLSCertificate 存储自动生成的 TLS 证书
func PutTLSCertificate(cert []byte) error {
	return Put(BucketRuntimeState, []byte("tls-cert"), cert)
}

// GetTLSKey 获取自动生成的 TLS 私钥
func GetTLSKey() ([]byte, error) {
	key, err := getView[[]byte](BucketRuntimeState, []byte("tls-key"))
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, nil
	}
	return *key, nil
}

// PutTLSKey 存储自动生成的 TLS 私钥
func PutTLSKey(key []byte) error {
	return Put(BucketRuntimeState, []byte("tls-key"), key)
}

// GetDHCPLenses 获取所有 DHCP 租约
func GetDHCPLeases() ([]DHCPLease, error) {
	leases, err := getView[[]DHCPLease](BucketRuntimeState, []byte("dhcp-leases"))
	if err != nil {
		return nil, err
	}
	if leases == nil {
		return []DHCPLease{}, nil
	}
	return *leases, nil
}

// PutDHCPLeases 存储所有 DHCP 租约
func PutDHCPLeases(leases []DHCPLease) error {
	return Put(BucketRuntimeState, []byte("dhcp-leases"), leases)
}

// GetPortBindings 获取所有端口绑定
func GetPortBindings() ([]PortBinding, error) {
	bindings, err := getView[[]PortBinding](BucketBindings, []byte("port-bindings"))
	if err != nil {
		return nil, err
	}
	if bindings == nil {
		return []PortBinding{}, nil
	}
	return *bindings, nil
}

// PutPortBindings 存储所有端口绑定
func PutPortBindings(bindings []PortBinding) error {
	return Put(BucketBindings, []byte("port-bindings"), bindings)
}
