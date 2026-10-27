package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
	"phaethon/config"
	"phaethon/db"
	"phaethon/util"
	"gopkg.in/yaml.v3"
)

// 命令行参数
var (
	dryRun  bool
	cleanup bool
)

// MigrationReport 迁移报告
type MigrationReport struct {
	Subscriptions []SubscriptionMigration `json:"subscriptions"`
	Rules         RulesMigration          `json:"rules"`
	RuntimeState  RuntimeStateMigration   `json:"runtime_state"`
	Packages      PackagesMigration       `json:"packages"`
	TLS           TLSMigration            `json:"tls"`
	InstanceState InstanceStateMigration  `json:"instance_state"`
	Timestamp     time.Time               `json:"timestamp"`
	DryRun        bool                    `json:"dry_run"`
}

type SubscriptionMigration struct {
	Name       string `json:"name"`
	SourceFile string `json:"source_file"`
	NodeCount  int    `json:"node_count"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
}

type RulesMigration struct {
	SourceFile string `json:"source_file"`
	RuleCount  int    `json:"rule_count"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
}

type RuntimeStateMigration struct {
	MeshIdentity bool `json:"mesh_identity"`
	ReverseID    bool `json:"reverse_id"`
	DHCPLeases   bool `json:"dhcp_leases"`
	PortBindings bool `json:"port_bindings"`
}

type PackagesMigration struct {
	Count   int `json:"count"`
	Success int `json:"success"`
	Failed  int `json:"failed"`
}

type TLSMigration struct {
	CertMigrated bool `json:"cert_migrated"`
	KeyMigrated  bool `json:"key_migrated"`
}

type InstanceStateMigration struct {
	SessionSecret bool `json:"session_secret"`
}

func init() {
	flag.BoolVar(&dryRun, "dry-run", false, "只检查不实际迁移")
	flag.BoolVar(&cleanup, "cleanup", false, "迁移后删除旧文件")
}

func main() {
	flag.Parse()

	fmt.Println("========================================")
	fmt.Println("数据迁移工具 v2.0")
	fmt.Println("========================================")
	if dryRun {
		fmt.Println("【干运行模式】只检查，不实际迁移")
	}
	fmt.Println("迁移源：旧文件（订阅缓存 + config.yaml + 运行时状态）")
	fmt.Println("迁移目标：数据库（统一存储）")
	fmt.Println()

	// 初始化数据库
	dbPath := filepath.Join(".", "data", "phaethon.db")
	fmt.Printf("初始化数据库：%s\n", dbPath)
	if !dryRun {
		if err := db.Init(dbPath); err != nil {
			fmt.Printf("❌ 数据库初始化失败：%v\n", err)
			os.Exit(1)
		}
		defer db.Close()
		fmt.Println("✅ 数据库初始化成功")
	} else {
		fmt.Println("【DRY-RUN】跳过数据库初始化")
	}
	fmt.Println()

	report := &MigrationReport{
		Timestamp: time.Now(),
		DryRun:    dryRun,
	}

	// 阶段 1：迁移订阅节点
	util.LogInfo("【阶段 1】迁移订阅节点...")
	migrateSubscriptions(report)

	// 阶段 2：迁移规则（可选）
	util.LogInfo("")
	util.LogInfo("【阶段 2】迁移规则...")
	migrateRules(report)

	// 阶段 3：迁移运行时状态
	util.LogInfo("")
	util.LogInfo("【阶段 3】迁移运行时状态...")
	migrateRuntimeState(report)

	// 阶段 4：迁移 Package 元数据
	util.LogInfo("")
	util.LogInfo("【阶段 4】迁移 Package 元数据...")
	migratePackageMetadata(report)

	// 阶段 5：验证
	util.LogInfo("")
	util.LogInfo("【阶段 5】验证迁移结果...")
	verifyMigration(report)

	// 阶段 6：清理（如果启用）
	if cleanup && !dryRun {
		util.LogInfo("")
		util.LogInfo("【阶段 6】清理旧文件...")
		cleanupOldFiles(report)
	}

	// 输出报告
	util.LogInfo("")
	util.LogInfo("========================================")
	if dryRun {
		util.LogInfo("干运行完成")
	} else {
		util.LogInfo("迁移完成")
	}
	util.LogInfo("========================================")

	reportJSON, _ := json.MarshalIndent(report, "", "  ")
	util.LogInfo("迁移报告：\n%s", string(reportJSON))
}

func migrateSubscriptions(report *MigrationReport) {
	// 旧缓存文件路径
	cacheDir := filepath.Join(".", "data", "cache", "subscription")

	// 检查目录是否存在
	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		util.LogWarn("订阅缓存目录不存在：%s", cacheDir)
		util.LogInfo("跳过订阅节点迁移")
		return
	}

	// 读取所有 .cache 文件
	files, err := filepath.Glob(filepath.Join(cacheDir, "*.cache"))
	if err != nil {
		util.LogError("扫描订阅缓存文件失败：%v", err)
		return
	}

	if len(files) == 0 {
		util.LogWarn("未找到订阅缓存文件")
		return
	}

	util.LogInfo("找到 %d 个订阅缓存文件", len(files))

	for _, file := range files {
		fileName := filepath.Base(file)
		subName := fileName[:len(fileName)-len(".cache")] // 去掉 .cache 后缀

		util.LogInfo("")
		util.LogInfo("处理订阅：%s (文件：%s)", subName, fileName)

		migration := SubscriptionMigration{
			Name:       subName,
			SourceFile: file,
		}

		// 读取文件内容
		content, err := os.ReadFile(file)
		if err != nil {
			migration.Error = fmt.Sprintf("读取文件失败：%v", err)
			util.LogError("  ❌ %s", migration.Error)
			report.Subscriptions = append(report.Subscriptions, migration)
			continue
		}

		util.LogInfo("  文件大小：%d bytes", len(content))

		// 解析订阅内容
 proxies, err := config.ParseSubscription(string(content))
		if err != nil {
			migration.Error = fmt.Sprintf("解析订阅内容失败：%v", err)
			util.LogError("  ❌ %s", migration.Error)
			report.Subscriptions = append(report.Subscriptions, migration)
			continue
		}

		if len(proxies) == 0 {
			migration.Error = "解析后节点数为 0"
			util.LogWarn("  ⚠️  %s", migration.Error)
			report.Subscriptions = append(report.Subscriptions, migration)
			continue
		}

		util.LogInfo("  解析到 %d 个节点", len(proxies))

		// 转换为 map[string]*config.Proxy
		proxyMap := make(map[string]*config.Proxy)
		for _, p := range proxies {
			if p.Name == "" {
				p.Name = fmt.Sprintf("%s:%d", p.Server, p.Port)
			}
			p.SourceGroup = subName
			proxyMap[p.Name] = p
		}

		// 写入数据库
		if dryRun {
			util.LogInfo("  【DRY-RUN】会迁移 %d 个节点到数据库", len(proxies))
			migration.NodeCount = len(proxies)
			migration.Success = true
		} else {
			if err := db.PutSubscriptionNodes(subName, proxyMap); err != nil {
				migration.Error = fmt.Sprintf("写入数据库失败：%v", err)
				util.LogError("  ❌ %s", migration.Error)
				report.Subscriptions = append(report.Subscriptions, migration)
				continue
			}

			migration.NodeCount = len(proxies)
			migration.Success = true
			util.LogInfo("  ✅ 迁移成功：%d 个节点", migration.NodeCount)
		}
		report.Subscriptions = append(report.Subscriptions, migration)
	}
}

func migrateRules(report *MigrationReport) {
	// 旧配置文件路径
	configFile := "config.yaml"

	// 检查文件是否存在
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		util.LogWarn("配置文件不存在：%s", configFile)
		util.LogInfo("跳过规则迁移")
		return
	}

	util.LogInfo("配置文件：%s", configFile)

	migration := RulesMigration{
		SourceFile: configFile,
	}

	// 读取配置文件
	data, err := os.ReadFile(configFile)
	if err != nil {
		migration.Error = fmt.Sprintf("读取配置文件失败：%v", err)
		util.LogError("❌ %s", migration.Error)
		report.Rules = migration
		return
	}

	util.LogInfo("文件大小：%d bytes", len(data))

	// 解析 YAML 提取 rules 字段
	var conf struct {
		Rules []string `yaml:"rules"`
	}
	if err := yaml.Unmarshal(data, &conf); err != nil {
		migration.Error = fmt.Sprintf("解析 YAML 失败：%v", err)
		util.LogError("❌ %s", migration.Error)
		report.Rules = migration
		return
	}

	if len(conf.Rules) == 0 {
		migration.Error = "配置文件中没有 rules 字段或为空"
		util.LogWarn("⚠️  %s", migration.Error)
		report.Rules = migration
		return
	}

	util.LogInfo("解析到 %d 条规则", len(conf.Rules))

	// 写入数据库
	if dryRun {
		util.LogInfo("【DRY-RUN】会迁移 %d 条规则到数据库", len(conf.Rules))
		migration.RuleCount = len(conf.Rules)
		migration.Success = true
	} else {
		// 写入数据库（使用 PutRules 一次性写入所有规则）
		if err := db.PutRules(conf.Rules); err != nil {
			migration.Error = fmt.Sprintf("写入规则失败：%v", err)
			util.LogError("❌ %s", migration.Error)
			report.Rules = migration
			return
		}

		migration.RuleCount = len(conf.Rules)
		migration.Success = true
		util.LogInfo("✅ 迁移成功：%d 条规则", migration.RuleCount)
	}
	report.Rules = migration
}

func migrateRuntimeState(report *MigrationReport) {
	dataDir := "."

	// 1. 迁移 mesh-state.json
	report.RuntimeState.MeshIdentity = migrateMeshState(dataDir)

	// 2. 迁移 reverse-id
	report.RuntimeState.ReverseID = migrateReverseID(dataDir)

	// 3. 迁移 dhcp-leases.json
	report.RuntimeState.DHCPLeases = migrateDHCPLeases(dataDir)

	// 4. 迁移 bindings.json
	report.RuntimeState.PortBindings = migrateBindings(dataDir)

	// 5. 迁移 admin-session-secret.key
	report.InstanceState.SessionSecret = migrateSessionSecret(dataDir)

	// 6. 迁移 admin-tls-cert.pem 和 admin-tls-key.pem
	migrateTLSCertKey(dataDir, report)
}

func migrateMeshState(dataDir string) bool {
	stateFile := filepath.Join(dataDir, "data", "state", "mesh-state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			util.LogInfo("  ⚠️  mesh-state.json 不存在，跳过")
			return false
		}
		util.LogError("  ❌ 读取 mesh-state.json 失败：%v", err)
		return false
	}

	var state struct {
		NodeID string `json:"nodeId"`
		Subnet string `json:"subnet,omitempty"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		util.LogError("  ❌ 解析 mesh-state.json 失败：%v", err)
		return false
	}

	if dryRun {
		util.LogInfo("  【DRY-RUN】会迁移 mesh-state.json → state/node-id + config/mesh.subnet (nodeId=%s, subnet=%s)", state.NodeID, state.Subnet)
		return true
	}

	// Save node-id to state bucket
	if err := db.PutNodeID(state.NodeID); err != nil {
		util.LogError("  ❌ 保存 node-id 到数据库失败：%v", err)
		return false
	}
	
	// Save subnet to MeshConfig
	cfg, err := db.GetMeshConfig()
	if err != nil {
		util.LogError("  ❌ 读取 mesh config 失败：%v", err)
		return false
	}
	if cfg == nil {
		cfg = &config.MeshConfig{}
	}
	cfg.Subnet = state.Subnet
	if err := db.PutMeshConfig(cfg); err != nil {
		util.LogError("  ❌ 保存 mesh config 到数据库失败：%v", err)
		return false
	}

	util.LogInfo("  ✅ mesh-state.json → state/node-id + config/mesh.subnet (nodeId=%s, subnet=%s)", state.NodeID, state.Subnet)
	return true
}

func migrateReverseID(dataDir string) bool {
	idFile := filepath.Join(dataDir, "data", "reverse-id")
	data, err := os.ReadFile(idFile)
	if err != nil {
		if os.IsNotExist(err) {
			util.LogInfo("  ⚠️  reverse-id 不存在，跳过")
			return false
		}
		util.LogError("  ❌ 读取 reverse-id 失败：%v", err)
		return false
	}

	id := string(data)
	if dryRun {
		util.LogInfo("  【DRY-RUN】会迁移 reverse-id → state/node-id (length=%d)", len(id))
		return true
	}

	// Save to state/node-id (only if not already set)
	existingID, err := db.GetNodeID()
	if err != nil {
		util.LogError("  ❌ 读取 node-id 失败：%v", err)
		return false
	}
	
	if existingID == "" {
		if err := db.PutNodeID(id); err != nil {
			util.LogError("  ❌ 保存 node-id 到数据库失败：%v", err)
			return false
		}
		util.LogInfo("  ✅ reverse-id → state/node-id (length=%d)", len(id))
	} else {
		util.LogInfo("  ⚠️  node-id 已存在 (%s)，跳过 reverse-id 迁移", existingID)
	}
	return true
}

func migrateDHCPLeases(dataDir string) bool {
	leaseFile := filepath.Join(dataDir, "data", "state", "dhcp-leases.json")
	data, err := os.ReadFile(leaseFile)
	if err != nil {
		if os.IsNotExist(err) {
			util.LogInfo("  ⚠️  dhcp-leases.json 不存在，跳过")
			return false
		}
		util.LogError("  ❌ 读取 dhcp-leases.json 失败：%v", err)
		return false
	}

	var entries []struct {
		IP        string    `json:"ip"`
		MAC       string    `json:"mac"`
		Hostname  string    `json:"hostname,omitempty"`
		ExpiresAt time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		util.LogError("  ❌ 解析 dhcp-leases.json 失败：%v", err)
		return false
	}

	leases := make([]db.DHCPLease, len(entries))
	for i, e := range entries {
		leases[i] = db.DHCPLease{
			IP:        e.IP,
			MAC:       e.MAC,
			Hostname:  e.Hostname,
			ExpiresAt: e.ExpiresAt,
		}
	}

	if dryRun {
		util.LogInfo("  【DRY-RUN】会迁移 dhcp-leases.json → runtime-state/dhcp-leases (count=%d)", len(leases))
		return true
	}

	if err := db.PutDHCPLeases(leases); err != nil {
		util.LogError("  ❌ 保存 DHCP leases 到数据库失败：%v", err)
		return false
	}

	util.LogInfo("  ✅ dhcp-leases.json → database (count=%d)", len(leases))
	return true
}

func migrateBindings(dataDir string) bool {
	bindingFile := filepath.Join(dataDir, "data", "state", "bindings.json")
	data, err := os.ReadFile(bindingFile)
	if err != nil {
		if os.IsNotExist(err) {
			util.LogInfo("  ⚠️  bindings.json 不存在，跳过")
			return false
		}
		util.LogError("  ❌ 读取 bindings.json 失败：%v", err)
		return false
	}

	var entries []struct {
		ReverseID      string    `json:"reverse_id"`
		Seq            int       `json:"seq"`
		Port           int       `json:"port"`
		ListenerProto  string    `json:"listener_proto"`
		Identity       string    `json:"identity"`
		DirectDstHost  string    `json:"direct_dst_host,omitempty"`
		DirectDstPort  int       `json:"direct_dst_port,omitempty"`
		RegistryProxy  string    `json:"registry_proxy,omitempty"`
		ControlAddr    string    `json:"control_addr"`
		RegistryAddr   string    `json:"registry_addr"`
		CreatedAt      time.Time `json:"created_at"`
		UpdatedAt      time.Time `json:"updated_at"`
		DisconnectedAt time.Time `json:"disconnected_at"`
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		util.LogError("  ❌ 解析 bindings.json 失败：%v", err)
		return false
	}

	bindings := make([]db.PortBinding, len(entries))
	for i, e := range entries {
		bindings[i] = db.PortBinding{
			ReverseID:      e.ReverseID,
			Seq:            e.Seq,
			Port:           e.Port,
			ListenerProto:  e.ListenerProto,
			Identity:       e.Identity,
			DirectDstHost:  e.DirectDstHost,
			DirectDstPort:  e.DirectDstPort,
			RegistryProxy:  e.RegistryProxy,
			ControlAddr:    e.ControlAddr,
			RegistryAddr:   e.RegistryAddr,
			CreatedAt:      e.CreatedAt,
			UpdatedAt:      e.UpdatedAt,
			DisconnectedAt: e.DisconnectedAt,
		}
	}

	if dryRun {
		util.LogInfo("  【DRY-RUN】会迁移 bindings.json → runtime-state/port-bindings (count=%d)", len(bindings))
		return true
	}

	if err := db.PutPortBindings(bindings); err != nil {
		util.LogError("  ❌ 保存 port bindings 到数据库失败：%v", err)
		return false
	}

	util.LogInfo("  ✅ bindings.json → database (count=%d)", len(bindings))
	return true
}

func migrateSessionSecret(dataDir string) bool {
	secretFile := filepath.Join(dataDir, "admin-session-secret.key")
	data, err := os.ReadFile(secretFile)
	if err != nil {
		if os.IsNotExist(err) {
			util.LogInfo("  ⚠️  admin-session-secret.key 不存在，跳过")
			return false
		}
		util.LogError("  ❌ 读取 admin-session-secret.key 失败：%v", err)
		return false
	}

	if len(data) != 32 {
		util.LogError("  ❌ admin-session-secret.key 长度不正确：%d (期望 32)", len(data))
		return false
	}

	if dryRun {
		util.LogInfo("  【DRY-RUN】会迁移 admin-session-secret.key → instance-state/session-secret (length=32)")
		return true
	}

	if err := db.PutSessionSecret(data); err != nil {
		util.LogError("  ❌ 保存 session secret 到数据库失败：%v", err)
		return false
	}

	util.LogInfo("  ✅ admin-session-secret.key → database (length=32)")
	return true
}

func migrateTLSCertKey(dataDir string, report *MigrationReport) {
	certFile := filepath.Join(dataDir, "admin-tls-cert.pem")
	keyFile := filepath.Join(dataDir, "admin-tls-key.pem")

	// 迁移证书
	certData, err := os.ReadFile(certFile)
	if err != nil {
		if os.IsNotExist(err) {
			util.LogInfo("  ⚠️  admin-tls-cert.pem 不存在，跳过")
		} else {
			util.LogError("  ❌ 读取 admin-tls-cert.pem 失败：%v", err)
		}
	} else if len(certData) > 0 {
		if dryRun {
			util.LogInfo("  【DRY-RUN】会迁移 admin-tls-cert.pem → instance-state/tls-cert (length=%d)", len(certData))
			report.TLS.CertMigrated = true
		} else {
			if err := db.PutTLSCertificate(certData); err != nil {
				util.LogError("  ❌ 保存 TLS cert 到数据库失败：%v", err)
			} else {
				util.LogInfo("  ✅ admin-tls-cert.pem → database (length=%d)", len(certData))
				report.TLS.CertMigrated = true
			}
		}
	}

	// 迁移私钥
	keyData, err := os.ReadFile(keyFile)
	if err != nil {
		if os.IsNotExist(err) {
			util.LogInfo("  ⚠️  admin-tls-key.pem 不存在，跳过")
		} else {
			util.LogError("  ❌ 读取 admin-tls-key.pem 失败：%v", err)
		}
	} else if len(keyData) > 0 {
		if dryRun {
			util.LogInfo("  【DRY-RUN】会迁移 admin-tls-key.pem → instance-state/tls-key (length=%d)", len(keyData))
			report.TLS.KeyMigrated = true
		} else {
			if err := db.PutTLSKey(keyData); err != nil {
				util.LogError("  ❌ 保存 TLS key 到数据库失败：%v", err)
			} else {
				util.LogInfo("  ✅ admin-tls-key.pem → database (length=%d)", len(keyData))
				report.TLS.KeyMigrated = true
			}
		}
	}
}

func migratePackageMetadata(report *MigrationReport) {
	packagesDir := filepath.Join(".", "data", "packages")

	// 检查目录是否存在
	if _, err := os.Stat(packagesDir); os.IsNotExist(err) {
		util.LogInfo("  ⚠️  packages 目录不存在，跳过")
		return
	}

	// 扫描所有 .json 文件
	files, err := filepath.Glob(filepath.Join(packagesDir, "*.json"))
	if err != nil {
		util.LogError("  ❌ 扫描 package 元数据文件失败：%v", err)
		return
	}

	if len(files) == 0 {
		util.LogInfo("  ⚠️  未找到 package 元数据文件")
		return
	}

	util.LogInfo("  找到 %d 个 package 元数据文件", len(files))
	report.Packages.Count = len(files)

	migrated := 0
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			util.LogError("  ❌ 读取 %s 失败：%v", file, err)
			report.Packages.Failed++
			continue
		}

		// 解析旧的 packageInfo 结构
		var oldInfo struct {
			ID             string              `json:"id"`
			Filename       string              `json:"filename"`
			Size           int64               `json:"size"`
			UploadedAt     time.Time           `json:"uploadedAt"`
			SignatureValid bool                `json:"signatureValid"`
			Meta           struct {
				Version   string `json:"version"`
				Platform  string `json:"platform"`
				Arch      string `json:"arch"`
				BuildTime string `json:"buildTime,omitempty"`
				GitCommit string `json:"gitCommit,omitempty"`
				GoVersion string `json:"goVersion,omitempty"`
				BuildTag  string `json:"buildTag,omitempty"`
			} `json:"meta"`
			Published bool `json:"published"`
		}

		if err := json.Unmarshal(data, &oldInfo); err != nil {
			util.LogError("  ❌ 解析 %s 失败：%v", file, err)
			report.Packages.Failed++
			continue
		}

		// 转换为 db.PackageMeta
		pkg := &db.PackageMeta{
			ID:             oldInfo.ID,
			Filename:       oldInfo.Filename,
			Size:           oldInfo.Size,
			UploadedAt:     oldInfo.UploadedAt,
			SignatureValid: oldInfo.SignatureValid,
			Version:        oldInfo.Meta.Version,
			Platform:       oldInfo.Meta.Platform,
			Arch:           oldInfo.Meta.Arch,
			BuildTime:      oldInfo.Meta.BuildTime,
			GitCommit:      oldInfo.Meta.GitCommit,
			GoVersion:      oldInfo.Meta.GoVersion,
			BuildTag:       oldInfo.Meta.BuildTag,
			Published:      oldInfo.Published,
		}

		if dryRun {
			util.LogInfo("  【DRY-RUN】会迁移 %s → packages/%s", file, pkg.ID)
			migrated++
			continue
		}

		if err := db.PutPackage(pkg); err != nil {
			util.LogError("  ❌ 保存 %s 到数据库失败：%v", file, err)
			report.Packages.Failed++
			continue
		}

		migrated++
	}

	report.Packages.Success = migrated
	if dryRun {
		util.LogInfo("  【DRY-RUN】会迁移 %d/%d 个 package 元数据", migrated, len(files))
	} else {
		util.LogInfo("  ✅ 迁移完成：%d/%d 个 package 元数据", migrated, len(files))
	}
}

func verifyMigration(report *MigrationReport) {
	if dryRun {
		util.LogInfo("【DRY-RUN】跳过验证")
		return
	}

	// 1. 验证订阅节点
	verifySubscriptions()

	// 2. 验证规则
	verifyRules()

	// 3. 验证运行时状态
	verifyRuntimeState()

	// 4. 验证包元数据
	verifyPackages()

	// 5. 验证 TLS
	verifyTLS()

	// 6. 验证实例状态
	verifyInstanceState()
}

func verifySubscriptions() {
	util.LogInfo("")
	util.LogInfo("验证订阅节点...")

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(db.BucketSubscriptionNodes)
		if b == nil {
			return fmt.Errorf("BucketSubscriptionNodes 不存在")
		}

		count := 0
		err := b.ForEach(func(k, v []byte) error {
			count++
			var nodes db.SubscriptionNodes
			if err := json.Unmarshal(v, &nodes); err != nil {
				return fmt.Errorf("反序列化失败 key=%s: %w", string(k), err)
			}
			util.LogInfo("  ✓ 订阅 %s: %d 个节点", string(k), len(nodes.Proxies))
			return nil
		})
		if err != nil {
			return err
		}

		util.LogInfo("数据库中共 %d 个订阅", count)
		return nil
	})

	if err != nil {
		util.LogError("验证订阅节点失败：%v", err)
	}
}

func verifyRules() {
	util.LogInfo("")
	util.LogInfo("验证规则...")

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(db.BucketRules)
		if b == nil {
			return fmt.Errorf("BucketRules 不存在")
		}

		// 规则存储为单个 JSON 数组（key="rules"）
		v := b.Get([]byte("rules"))
		if v == nil {
			util.LogWarn("  ⚠️  规则数据不存在")
			return nil
		}

		var rules []string
		if err := json.Unmarshal(v, &rules); err != nil {
			return fmt.Errorf("反序列化规则失败：%w", err)
		}

		util.LogInfo("  ✓ 规则数量：%d 条", len(rules))
		for i, rule := range rules {
			if i < 5 { // 只显示前 5 条
				util.LogInfo("    [%d] %s", i, rule)
			} else if i == 5 {
				util.LogInfo("    ... (省略后续 %d 条)", len(rules)-5)
				break
			}
		}
		return nil
	})

	if err != nil {
		util.LogError("验证规则失败：%v", err)
	}
}

func verifyRuntimeState() {
	util.LogInfo("")
	util.LogInfo("验证运行时状态...")

	// 验证 node-id (from state bucket)
	nodeID, err := db.GetNodeID()
	if err != nil {
		util.LogError("  ❌ 读取 node-id 失败：%v", err)
	} else if nodeID != "" {
		util.LogInfo("  ✓ node-id: %s", nodeID)
	} else {
		util.LogWarn("  ⚠️  node-id 不存在")
	}

	// 验证 mesh config (subnet)
	meshCfg, err := db.GetMeshConfig()
	if err != nil {
		util.LogError("  ❌ 读取 mesh config 失败：%v", err)
	} else if meshCfg != nil {
		util.LogInfo("  ✓ mesh config: subnet=%s", meshCfg.Subnet)
	} else {
		util.LogWarn("  ⚠️  mesh config 不存在")
	}

	// 验证 dhcp-leases
	leases, err := db.GetDHCPLeases()
	if err != nil {
		util.LogError("  ❌ 读取 dhcp-leases 失败：%v", err)
	} else {
		util.LogInfo("  ✓ dhcp-leases: count=%d", len(leases))
	}

	// 验证 port-bindings
	bindings, err := db.GetPortBindings()
	if err != nil {
		util.LogError("  ❌ 读取 port-bindings 失败：%v", err)
	} else {
		util.LogInfo("  ✓ port-bindings: count=%d", len(bindings))
	}
}

func verifyPackages() {
	util.LogInfo("")
	util.LogInfo("验证包元数据...")

	packages, err := db.ListPackages()
	if err != nil {
		util.LogError("  ❌ 读取 packages 失败：%v", err)
		return
	}

	util.LogInfo("  ✓ packages: count=%d", len(packages))
	for _, pkg := range packages {
		util.LogInfo("    - %s/%s@%s (id=%s)",
			pkg.Platform, pkg.Arch, pkg.Version, pkg.ID)
	}
}

func verifyTLS() {
	util.LogInfo("")
	util.LogInfo("验证 TLS 证书...")

	cert, err := db.GetTLSCertificate()
	if err != nil {
		util.LogError("  ❌ 读取 TLS cert 失败：%v", err)
	} else if len(cert) > 0 {
		util.LogInfo("  ✓ TLS cert: length=%d", len(cert))
	} else {
		util.LogWarn("  ⚠️  TLS cert 不存在")
	}

	key, err := db.GetTLSKey()
	if err != nil {
		util.LogError("  ❌ 读取 TLS key 失败：%v", err)
	} else if len(key) > 0 {
		util.LogInfo("  ✓ TLS key: length=%d", len(key))
	} else {
		util.LogWarn("  ⚠️  TLS key 不存在")
	}
}

func verifyInstanceState() {
	util.LogInfo("")
	util.LogInfo("验证实例状态...")

	// 验证 session-secret
	secret, err := db.GetSessionSecret()
	if err != nil {
		util.LogError("  ❌ 读取 session-secret 失败：%v", err)
	} else if len(secret) > 0 {
		util.LogInfo("  ✓ session-secret: length=%d", len(secret))
	} else {
		util.LogWarn("  ⚠️  session-secret 不存在")
	}
}

func cleanupOldFiles(report *MigrationReport) {
	dataDir := "."

	// 清理订阅缓存
	cacheDir := filepath.Join(".", "data", "cache", "subscription")
	if len(report.Subscriptions) > 0 {
		util.LogInfo("清理订阅缓存目录：%s", cacheDir)
		if err := os.RemoveAll(cacheDir); err != nil {
			util.LogError("  ❌ 清理失败：%v", err)
		} else {
			util.LogInfo("  ✅ 已清理")
		}
	}

	// 清理运行时状态文件
	filesToClean := []struct {
		path     string
		migrated bool
	}{
		{filepath.Join(dataDir, "data", "state", "mesh-state.json"), report.RuntimeState.MeshIdentity},
		{filepath.Join(dataDir, "data", "reverse-id"), report.RuntimeState.ReverseID},
		{filepath.Join(dataDir, "data", "state", "dhcp-leases.json"), report.RuntimeState.DHCPLeases},
		{filepath.Join(dataDir, "data", "state", "bindings.json"), report.RuntimeState.PortBindings},
		{filepath.Join(dataDir, "admin-session-secret.key"), report.InstanceState.SessionSecret},
		{filepath.Join(dataDir, "admin-tls-cert.pem"), report.TLS.CertMigrated},
		{filepath.Join(dataDir, "admin-tls-key.pem"), report.TLS.KeyMigrated},
	}

	for _, f := range filesToClean {
		if !f.migrated {
			continue
		}
		util.LogInfo("清理旧文件：%s", f.path)
		if err := os.Remove(f.path); err != nil {
			if !os.IsNotExist(err) {
				util.LogError("  ❌ 清理失败：%v", err)
			}
		} else {
			util.LogInfo("  ✅ 已清理")
		}
	}

	// 清理 package 元数据文件（.json，保留 .pkg）
	if report.Packages.Success > 0 {
		packagesDir := filepath.Join(".", "data", "packages")
		files, _ := filepath.Glob(filepath.Join(packagesDir, "*.json"))
		for _, file := range files {
			util.LogInfo("清理旧文件：%s", file)
			if err := os.Remove(file); err != nil {
				util.LogError("  ❌ 清理失败：%v", err)
			} else {
				util.LogInfo("  ✅ 已清理")
			}
		}
	}
}
