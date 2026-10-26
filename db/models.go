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

// PackageMeta 包元数据（对应 admin/package.go 的 packageInfo）
type PackageMeta struct {
	ID             string    `json:"id"`
	Filename       string    `json:"filename"`
	Size           int64     `json:"size"`
	UploadedAt     time.Time `json:"uploadedAt"`
	SignatureValid bool      `json:"signatureValid"`
	// 嵌入 signing.PackageMeta 的字段
	Version   string `json:"version"`
	Platform  string `json:"platform"`
	Arch      string `json:"arch"`
	BuildTime string `json:"buildTime,omitempty"`
	GitCommit string `json:"gitCommit,omitempty"`
	GoVersion string `json:"goVersion,omitempty"`
	BuildTag  string `json:"buildTag,omitempty"`
	Published bool   `json:"published"`
}
