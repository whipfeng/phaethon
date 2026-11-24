package db

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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
//
// 必须在真正持有操作员控制台的进程里调用（目前是 supervisor/watchdog）。
// worker 的 stdout 是 watchdog 的按行协议管道：不带换行的提示永远不会
// 被转发到控制台，用户会面对"看不见的问题"一路回车，最终静默使用默认值。
func InteractiveInit() error {
	return interactiveInit(bufio.NewReader(os.Stdin), os.Stdout, term.IsTerminal(int(os.Stdin.Fd())))
}

// errInputClosed 表示 stdin 在向导中途结束（EOF / Ctrl+Z）。
// 这种情况必须中止初始化，绝不能把"读不到输入"当成用户接受了默认值。
var errInputClosed = errors.New("输入流已结束，初始化已取消（未写入任何配置）")

func interactiveInit(reader *bufio.Reader, out io.Writer, silentPassword bool) error {
	fmt.Fprintln(out, "首次启动，进入初始化模式（直接回车接受方括号中的默认值）")

	// 1/4 监听地址（D2）：默认仅回环，公网暴露需要用户显式选择。
	bindAddr := "127.0.0.1"
	fmt.Fprintln(out)
	fmt.Fprintln(out, "[1/4] Admin 监听地址：")
	fmt.Fprintln(out, "  1) 127.0.0.1  仅本机可访问（推荐）")
	fmt.Fprintln(out, "  2) 0.0.0.0    允许其他机器访问")
	fmt.Fprintln(out, "  3) ::1        IPv6 回环")
	for {
		line, err := readAnswer(reader, out, "请选择 [1]: ")
		if err != nil {
			return err
		}
		switch line {
		case "", "1", "127.0.0.1":
			bindAddr = "127.0.0.1"
		case "2", "0.0.0.0":
			bindAddr = "0.0.0.0"
		case "3", "::1":
			bindAddr = "::1"
		default:
			fmt.Fprintln(out, "  只能选择 1/2/3，请重新输入")
			continue
		}
		break
	}

	// 2/4 Admin 端口：校验数字/范围，并用所选监听地址探测是否可绑定。
	port := defaultAdminPort
	for {
		line, err := readAnswer(reader, out, fmt.Sprintf("[2/4] Admin API 端口 [%d]: ", port))
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		p, parseErr := strconv.Atoi(line)
		if parseErr != nil || p < 1 || p > 65535 {
			fmt.Fprintln(out, "  端口必须是 1-65535 的数字，请重新输入")
			continue
		}
		ln, listenErr := net.Listen("tcp", net.JoinHostPort(bindAddr, line))
		if listenErr != nil {
			fmt.Fprintf(out, "  端口 %d 在 %s 上无法绑定: %v，请重新输入\n", p, bindAddr, listenErr)
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

	// 3/4 认证（可选）：密码静默输入，非空、长度达标且两次一致。
	authStr, err := readAnswer(reader, out, "[3/4] 是否设置 Admin 认证？(y/N): ")
	if err != nil {
		return err
	}
	authStr = strings.ToLower(authStr)

	if authStr == "y" || authStr == "yes" {
		username, err := readAnswer(reader, out, "      用户名 [admin]: ")
		if err != nil {
			return err
		}
		if username == "" {
			username = "admin"
		}

		for {
			pw, err := readPasswordSilent(reader, out, silentPassword, "      密码（至少 8 位）: ")
			if err != nil {
				return err
			}
			if pw == "" {
				fmt.Fprintln(out, "  密码不能为空，请重新输入")
				continue
			}
			if len(pw) < adminPasswordMinLen {
				fmt.Fprintf(out, "  密码至少 %d 个字符，请重新输入\n", adminPasswordMinLen)
				continue
			}
			pw2, err := readPasswordSilent(reader, out, silentPassword, "      再次输入密码: ")
			if err != nil {
				return err
			}
			if pw != pw2 {
				fmt.Fprintln(out, "  两次输入不一致，请重新输入")
				continue
			}
			adminConfig.AuthEnabled = true
			adminConfig.Username = username
			adminConfig.Password = pw
			break
		}
	} else {
		// D2：不启用认证必须给出明确风险提示。
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  ⚠ 已选择不启用认证：")
		fmt.Fprintln(out, "    - 任何能访问该端口的人都可以修改全部配置、读取会话密钥与证书")
		fmt.Fprintln(out, "    - 若监听 0.0.0.0，等于把管理面完全暴露在网络上")
		fmt.Fprintln(out, "    - 建议之后尽快在控制台设置认证；仅本机调试可使用 127.0.0.1")
		fmt.Fprintln(out)
	}

	// 4/4 Mesh node-id（必填，留空自动生成；格式校验：DNS 标签安全）。
	var nodeID string
	for {
		line, err := readAnswer(reader, out, "[4/4] Mesh node-id [留空自动生成]: ")
		if err != nil {
			return err
		}
		if line == "" {
			nodeID = generateNodeID()
			fmt.Fprintf(out, "生成 Mesh node-id: %s\n", nodeID)
			break
		}
		if err := validateNodeID(line); err != nil {
			fmt.Fprintf(out, "  %v，请重新输入\n", err)
			continue
		}
		nodeID = line
		break
	}

	// 一次性事务写入，避免中途退出产生半成品配置。
	if err := InitConfigs(adminConfig, &config.MeshConfig{NodeID: nodeID}); err != nil {
		return fmt.Errorf("保存初始化配置失败: %w", err)
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "初始化完成！")
	fmt.Fprintln(out, "请通过 Admin API 或控制台 UI 管理配置（代理、映射、规则等）")

	return nil
}

// readAnswer 打印提示并读取一行。EOF 且没有任何输入时返回 errInputClosed，
// 让调用方中止而不是静默采用默认值。
func readAnswer(reader *bufio.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if strings.TrimSpace(line) == "" {
				fmt.Fprintln(out)
				return "", errInputClosed
			}
		} else {
			return "", fmt.Errorf("读取输入失败: %w", err)
		}
	}
	return strings.TrimSpace(line), nil
}

// readPasswordSilent 从终端静默读取密码（不回显）；非终端环境退化为普通读取。
// 复用外层 reader，避免 bufio 预读缓冲导致后续输入被跳过。
func readPasswordSilent(reader *bufio.Reader, out io.Writer, silentPassword bool, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	if silentPassword {
		buf, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return "", fmt.Errorf("读取密码失败: %w", err)
		}
		return strings.TrimSpace(string(buf)), nil
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) {
			if strings.TrimSpace(line) == "" {
				fmt.Fprintln(out)
				return "", errInputClosed
			}
		} else {
			return "", fmt.Errorf("读取输入失败: %w", err)
		}
	}
	return strings.TrimSpace(line), nil
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
