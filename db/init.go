package db

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	bolt "go.etcd.io/bbolt"

	"phaethon/config"
)

const defaultAdminPort = 39999

// InteractiveInit 交互式初始化（配置为空时）。
// 所有答案收集完毕后才一次性事务写入，中途退出不会留下半成品配置。
func InteractiveInit() error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("首次启动，进入初始化模式（全部直接回车可使用默认值）")
	fmt.Println()

	// Admin 端口：校验数字/范围，并探测端口是否可绑定
	port := defaultAdminPort
	for {
		fmt.Printf("Admin API 端口 (默认 %d): ", port)
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		p, err := strconv.Atoi(line)
		if err != nil || p < 1 || p > 65535 {
			fmt.Println("  端口必须是 1-65535 的数字，请重新输入")
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", line))
		if err != nil {
			fmt.Printf("  端口 %d 无法绑定: %v，请重新输入\n", p, err)
			continue
		}
		ln.Close()
		port = p
		break
	}

	adminConfig := &config.AdminConfig{
		Enabled: true,
		Addr:    net.JoinHostPort("0.0.0.0", strconv.Itoa(port)),
	}

	// 认证（可选）：密码静默输入，非空且两次一致
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

		for {
			pw := readPasswordSilent(reader, "密码: ")
			if pw == "" {
				fmt.Println("  密码不能为空，请重新输入")
				continue
			}
			pw2 := readPasswordSilent(reader, "再次输入密码: ")
			if pw != pw2 {
				fmt.Println("  两次输入不一致，请重新输入")
				continue
			}
			adminConfig.AuthEnabled = true
			adminConfig.Username = username
			adminConfig.Password = pw
			break
		}
	}

	// Mesh node-id（必填，留空自动生成）
	fmt.Print("Mesh node-id (留空自动生成): ")
	nodeID, _ := reader.ReadString('\n')
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		nodeID = generateNodeID()
		fmt.Printf("生成 Mesh node-id: %s\n", nodeID)
	}

	// 一次性事务写入，避免中途退出产生半成品配置
	if err := InitConfigs(adminConfig, &config.MeshConfig{NodeID: nodeID}); err != nil {
		return fmt.Errorf("保存初始化配置失败: %w", err)
	}

	fmt.Println()
	fmt.Println("初始化完成！")
	fmt.Println("请通过 Admin API 或控制台 UI 管理配置（代理、映射、规则等）")

	return nil
}

// readPasswordSilent 从终端静默读取密码（不回显）；非终端环境退化为普通读取。
// 复用外层 reader，避免 bufio 预读缓冲导致后续输入被跳过。
func readPasswordSilent(reader *bufio.Reader, prompt string) string {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		buf, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return strings.TrimSpace(string(buf))
	}
	line, _ := reader.ReadString('\n')
	return strings.TrimSpace(line)
}

// InitConfigs 在单个事务中写入 Admin 与 Mesh 初始配置。
func InitConfigs(admin *config.AdminConfig, mesh *config.MeshConfig) error {
	return Update(func(tx *bolt.Tx) error {
		if err := putJSON(tx.Bucket(BucketAdminConfig), keyAdmin, admin); err != nil {
			return err
		}
		return putJSON(tx.Bucket(BucketMeshConfig), keyMesh, mesh)
	})
}

// generateNodeID 生成随机 node-id
func generateNodeID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// ImportYAML has been removed. Configuration is now managed through the admin API
// using fine-grained operations (PutProxy, PutMapping, etc.).

// InitDefault 使用默认配置初始化（非交互式）。
// 调用方须在初始化后提示用户：默认配置无认证，公网部署应立即设置。
func InitDefault() error {
	return InitConfigs(
		&config.AdminConfig{
			Enabled: true,
			Addr:    net.JoinHostPort("0.0.0.0", strconv.Itoa(defaultAdminPort)),
		},
		&config.MeshConfig{NodeID: generateNodeID()},
	)
}
