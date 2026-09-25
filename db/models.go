package db

import "time"

// 配置类型统一使用 config 包的 Proxy/ProxyGroup/Subscription/Mapping/
// Resolver/ReverseConfig/AdminConfig/TUNConfig/MeshConfig，本文件只保留
// 配置之外的状态与历史数据模型。

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
