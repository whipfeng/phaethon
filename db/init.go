package db

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"phaethon/config"
)

// InteractiveInit 交互式初始化（首次启动、数据库为空时）
func InteractiveInit() error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("首次启动，进入初始化模式")
	fmt.Println()

	// Admin 配置
	fmt.Print("Admin API 端口 (默认 39999): ")
	portStr, _ := reader.ReadString('\n')
	portStr = strings.TrimSpace(portStr)
	if portStr == "" {
		portStr = "39999"
	}

	adminConfig := &config.AdminConfig{
		Enabled: true,
		Addr:    "0.0.0.0:" + portStr,
	}

	// 询问是否设置认证
	fmt.Print("是否设置 Admin 认证？(y/N): ")
	authStr, _ := reader.ReadString('\n')
	authStr = strings.TrimSpace(strings.ToLower(authStr))

	if authStr == "y" || authStr == "yes" {
		fmt.Print("用户名 (默认 admin): ")
		username, _ := reader.ReadString('\n')
		username = strings.TrimSpace(username)
		if username == "" {
			username = "admin"
		}

		fmt.Print("密码: ")
		password, _ := reader.ReadString('\n')
		password = strings.TrimSpace(password)

		adminConfig.AuthEnabled = true
		adminConfig.Username = username
		adminConfig.Password = password
	}

	if err := PutAdminConfig(adminConfig); err != nil {
		return fmt.Errorf("保存 Admin 配置失败: %w", err)
	}

	// Mesh 配置（Mesh 必须启用，node-id 必填）
	fmt.Print("Mesh node-id (留空自动生成): ")
	nodeID, _ := reader.ReadString('\n')
	nodeID = strings.TrimSpace(nodeID)

	if nodeID == "" {
		nodeID = generateNodeID()
		fmt.Printf("生成 Mesh node-id: %s\n", nodeID)
	}

	if err := PutMeshConfig(&config.MeshConfig{NodeID: nodeID}); err != nil {
		return fmt.Errorf("保存 Mesh 配置失败: %w", err)
	}

	// 询问是否导入现有配置
	fmt.Print("是否导入现有配置？(y/N): ")
	importStr, _ := reader.ReadString('\n')
	importStr = strings.TrimSpace(strings.ToLower(importStr))

	if importStr == "y" || importStr == "yes" {
		fmt.Print("配置文件路径: ")
		configPath, _ := reader.ReadString('\n')
		configPath = strings.TrimSpace(configPath)

		if err := ImportYAML(configPath); err != nil {
			fmt.Printf("导入配置失败: %v\n", err)
			fmt.Println("将使用当前已保存的配置启动")
		} else {
			fmt.Println("导入配置成功")
		}
	}

	fmt.Println()
	fmt.Println("初始化完成！")

	return nil
}

// generateNodeID 生成随机 node-id
func generateNodeID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// ImportYAML 从 YAML 配置文件导入到数据库（全量替换）
func ImportYAML(path string) error {
	conf, err := config.LoadRaw(path)
	if err != nil {
		return fmt.Errorf("读取配置失败: %w", err)
	}
	return ImportRuleConf(conf)
}

// InitDefault 使用默认配置初始化（非交互式）
func InitDefault() error {
	if err := PutAdminConfig(&config.AdminConfig{
		Enabled: true,
		Addr:    "0.0.0.0:39999",
	}); err != nil {
		return err
	}

	if err := PutMeshConfig(&config.MeshConfig{
		NodeID: generateNodeID(),
	}); err != nil {
		return err
	}

	return nil
}
