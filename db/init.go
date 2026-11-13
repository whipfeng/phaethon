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

// adminPasswordMinLen 是交互式初始化设置密码时的最小长度（D2）。
const adminPasswordMinLen = 8

// InteractiveInit 交互式初始化（配置为空时）。
// 所有答案收集完毕后才一次性事务写入，中途退出不会留下半成品配置。
func InteractiveInit() error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("首次启动，进入初始化模式（全部直接回车可使用默认值）")
	fmt.Println()

	// 监听地址（D2）：默认仅回环，公网暴露需要用户显式选择。
	bindAddr := "127.0.0.1"
	for {
		fmt.Printf("Admin 监听地址 (默认 127.0.0.1，可选 0.0.0.0 表示对外开放): ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		switch line {
		case "127.0.0.1", "0.0.0.0", "::1":
			bindAddr = line
		default:
			fmt.Println("  只支持 127.0.0.1 / 0.0.0.0 / ::1，请重新输入")
			continue
		}
		break
	}

	// Admin 端口：校验数字/范围，并用所选监听地址探测是否可绑定
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
		ln, err := net.Listen("tcp", net.JoinHostPort(bindAddr, line))
		if err != nil {
			fmt.Printf("  端口 %d 在 %s 上无法绑定: %v，请重新输入\n", p, bindAddr, err)
			continue
		}
		ln.Close()
		port = p
		break
	}

	adminConfig := &config.AdminConfig{
		Enabled: true,
		Addr:    net.JoinHostPort(bindAddr, strconv.Itoa(port)),
	}

	// 认证（可选）：密码静默输入，非空、长度达标且两次一致
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
			if len(pw) < adminPasswordMinLen {
				fmt.Printf("  密码至少 %d 个字符，请重新输入\n", adminPasswordMinLen)
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
	} else {
		// D2：不启用认证必须给出明确风险提示。
		fmt.Println()
		fmt.Println("  ⚠ 已选择不启用认证：")
		fmt.Println("    - 任何能访问该端口的人都可以修改全部配置、读取会话密钥与证书")
		fmt.Println("    - 若监听 0.0.0.0，等于把管理面完全暴露在网络上")
		fmt.Println("    - 建议之后尽快在控制台设置认证；仅本机调试可使用 127.0.0.1")
		fmt.Println()
	}

	// Mesh node-id（必填，留空自动生成；格式校验：DNS 标签安全）
	var nodeID string
	for {
		fmt.Print("Mesh node-id (留空自动生成): ")
		line, _ := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			nodeID = generateNodeID()
			fmt.Printf("生成 Mesh node-id: %s\n", nodeID)
			break
		}
		if err := validateNodeID(line); err != nil {
			fmt.Printf("  %v，请重新输入\n", err)
			continue
		}
		nodeID = line
		break
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

// validateNodeID enforces a DNS-label-safe mesh node id: 1-63 chars of
// [a-z0-9-], not starting or ending with '-'. The node id becomes part of
// "<nodeid>.phn" domains, so anything else breaks resolution (D2).
func validateNodeID(id string) error {
	if id == "" {
		return fmt.Errorf("node-id 不能为空")
	}
	if len(id) > 63 {
		return fmt.Errorf("node-id 最长 63 个字符")
	}
	if id[0] == '-' || id[len(id)-1] == '-' {
		return fmt.Errorf("node-id 不能以 '-' 开头或结尾")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return fmt.Errorf("node-id 只能包含小写字母、数字和 '-'（发现 %q）", string(c))
	}
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

// InitDefault 使用默认配置初始化（非交互式）。
// 调用方须在初始化后提示用户：默认配置无认证，公网部署应立即设置。
func InitDefault() error {
	return InitConfigs(
		&config.AdminConfig{
			Enabled:     true,
			Addr:        net.JoinHostPort("0.0.0.0", strconv.Itoa(defaultAdminPort)),
			AuthEnabled: true, // secure-by-default; main generates a random initial password
			Username:    "admin",
		},
		&config.MeshConfig{NodeID: generateNodeID()},
	)
}
