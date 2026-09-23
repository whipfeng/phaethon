package db

import "time"

// ProxyConfig 代理配置
type ProxyConfig struct {
	Name                 string `json:"name"`
	Type                 string `json:"type"`
	Server               string `json:"server,omitempty"`
	Port                 int    `json:"port,omitempty"`
	Username             string `json:"username,omitempty"`
	Password             string `json:"password,omitempty"`
	PrivateKey           string `json:"private-key,omitempty"`
	PrivateKeyPassphrase string `json:"private-key-passphrase,omitempty"`
	UUID                 string `json:"uuid,omitempty"`
	Sni                  string `json:"sni,omitempty"`
	Servername           string `json:"servername,omitempty"`
	SkipCertVerify       bool   `json:"skip-cert-verify,omitempty"`
	UDP                  bool   `json:"udp,omitempty"`
	P2P                  bool   `json:"p2p,omitempty"`
	Cipher               string `json:"cipher,omitempty"`
	Tfo                  bool   `json:"tfo,omitempty"`
	URL                  string `json:"url,omitempty"`
	ViaProxy             string `json:"via,omitempty"`
	ReverseAddress       string `json:"reverse-address,omitempty"`
	Enabled              *bool  `json:"enabled,omitempty"`
}

// RuleConfig 规则配置
type RuleConfig struct {
	Index int    `json:"index"`
	Type  string `json:"type"`  // DOMAIN-SUFFIX, IP-CIDR, etc.
	Value string `json:"value"`
	Proxy string `json:"proxy"`
}

// MeshConfig Mesh 配置
type MeshConfig struct {
	NodeID string `json:"node_id"`
	Subnet string `json:"subnet,omitempty"`
	VIP    string `json:"vip,omitempty"`
}

// AdminConfig Admin 配置
type AdminConfig struct {
	Listen string          `json:"listen"`
	Auth   *AdminAuthConfig `json:"auth,omitempty"`
}

// AdminAuthConfig Admin 认证配置
type AdminAuthConfig struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// FakeIPEntry Fake-IP 映射条目
type FakeIPEntry struct {
	IP        string    `json:"ip"`
	Domain    string    `json:"domain"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
}

// PackageMeta 包元数据
type PackageMeta struct {
	Platform  string    `json:"platform"`
	Arch      string    `json:"arch"`
	Version   string    `json:"version"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	Hash      string    `json:"hash"`
	CreatedAt time.Time `json:"created_at"`
	Published bool      `json:"published"`
}

// PeerInfo Peer 信息
type PeerInfo struct {
	NodeID   string    `json:"node_id"`
	VIP      string    `json:"vip"`
	LastSeen time.Time `json:"last_seen"`
	Status   string    `json:"status"`
	Version  string    `json:"version,omitempty"`
}

// ConnectionLog 连接日志
type ConnectionLog struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Src       string    `json:"src"`
	Dst       string    `json:"dst"`
	Proxy     string    `json:"proxy"`
	Protocol  string    `json:"protocol"`
	Duration  float64   `json:"duration"`
	BytesSent int64     `json:"bytes_sent"`
	BytesRecv int64     `json:"bytes_recv"`
}
