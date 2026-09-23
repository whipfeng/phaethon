package db

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// InteractiveInit 交互式初始化
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
	
	adminConfig := &AdminConfig{
		Listen: ":" + portStr,
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
		
		adminConfig.Auth = &AdminAuthConfig{
			Username: username,
			Password: password,
		}
	}
	
	// 保存 Admin 配置
	err := Put(BucketConfig, []byte("admin"), adminConfig)
	if err != nil {
		return fmt.Errorf("保存 Admin 配置失败: %w", err)
	}
	
	// Mesh 配置
	fmt.Print("Mesh node-id (留空自动生成): ")
	nodeID, _ := reader.ReadString('\n')
	nodeID = strings.TrimSpace(nodeID)
	
	if nodeID == "" {
		// 自动生成 node-id
		nodeID = generateNodeID()
		fmt.Printf("生成 Mesh node-id: %s\n", nodeID)
	}
	
	meshConfig := &MeshConfig{
		NodeID: nodeID,
	}
	
	// 保存 Mesh 配置
	err = Put(BucketConfig, []byte("mesh"), meshConfig)
	if err != nil {
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
		
		err := importFromYAML(configPath)
		if err != nil {
			fmt.Printf("导入配置失败: %v\n", err)
			fmt.Println("将使用空配置启动")
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

// importFromYAML 从 YAML 文件导入配置
func importFromYAML(path string) error {
	// TODO: 实现 YAML 导入逻辑
	// 需要解析 config.yaml 并导入到数据库
	return fmt.Errorf("YAML 导入功能尚未实现")
}

// InitDefault 使用默认配置初始化（非交互式）
func InitDefault() error {
	// 默认 Admin 配置
	adminConfig := &AdminConfig{
		Listen: ":39999",
	}
	
	err := Put(BucketConfig, []byte("admin"), adminConfig)
	if err != nil {
		return err
	}
	
	// 默认 Mesh 配置
	meshConfig := &MeshConfig{
		NodeID: generateNodeID(),
	}
	
	err = Put(BucketConfig, []byte("mesh"), meshConfig)
	if err != nil {
		return err
	}
	
	return nil
}
