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

	fmt.Println()
	fmt.Println("初始化完成！")
	fmt.Println("请通过 Admin API 或控制台 UI 管理配置（代理、映射、规则等）")

	return nil
}

// generateNodeID 生成随机 node-id
func generateNodeID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// ImportYAML has been removed. Configuration is now managed through the admin API
// using fine-grained operations (PutProxy, PutMapping, etc.).

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
