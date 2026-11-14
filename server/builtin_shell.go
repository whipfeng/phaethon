package server

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/crypto/ssh"

	"phaethon/util"
)

// runBuiltinShell runs phaethon's own interactive shell over the SSH channel.
// It is used only when no shell was explicitly configured and the auto-detected
// system shell cannot be given a PTY; it deliberately does not wrap an arbitrary
// program (vim/top/ssh still require a real PTY).
func runBuiltinShell(channel ssh.Channel, cols, rows uint32, reason string) {
	if cols == 0 {
		cols = 100
	}
	if rows == 0 {
		rows = 30
	}
	model := newBuiltinShellModel(int(cols), int(rows), reason)
	program := tea.NewProgram(
		model,
		tea.WithInput(&lfToCRReader{r: channel}),
		tea.WithOutput(channel),
		tea.WithAltScreen(),
	)
	if _, err := program.Run(); err != nil {
		util.LogModuleWarn("ssh", "built-in shell exited: %v", err)
	}
	sendExitStatus(channel, 0)
}

// lfToCRReader normalizes line endings for the built-in shell. A terminal
// sends CR for Enter, but non-PTY clients (ssh -T, scripts) pipe plain LF;
// Bubble Tea's key decoder does not treat LF as Enter, so without this every
// piped line would be concatenated into one input line.
type lfToCRReader struct{ r io.Reader }

func (l *lfToCRReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	for i := 0; i < n; i++ {
		if p[i] == '\n' {
			p[i] = '\r'
		}
	}
	return n, err
}

type builtinShellModel struct {
	input    textinput.Model
	viewport viewport.Model

	history []string
	histIdx int
	cwd     string
	output  []string
	width   int
	height  int
	reason  string
}

func newBuiltinShellModel(cols, rows int, reason string) builtinShellModel {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		cwd = defaultWorkDir()
	}
	input := textinput.New()
	input.Placeholder = "输入命令，help 查看帮助"
	input.Prompt = "> "
	input.CharLimit = 4096
	input.Width = cols - 4
	input.Focus()

	vp := viewport.New(cols, rows-4)
	m := builtinShellModel{
		input:    input,
		viewport: vp,
		cwd:      cwd,
		width:    cols,
		height:   rows,
		reason:   reason,
	}
	m.appendOutput("phaethon built-in shell")
	if reason != "" {
		m.appendOutput("提示: " + reason)
	}
	m.appendOutput("输入 help 查看命令；!<命令> 调用系统 shell；exit 退出。")
	m.refreshViewport()
	return m
}

func (m builtinShellModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m builtinShellModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width = msg.Width
		if h := msg.Height - 4; h > 3 {
			m.viewport.Height = h
		}
		m.input.Width = msg.Width - 4
		m.refreshViewport()
		return m, nil

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			return m, tea.Quit
		case tea.KeyEnter:
			line := strings.TrimSpace(m.input.Value())
			if line == "" {
				return m, nil
			}
			m.input.SetValue("")
			m.history = append(m.history, line)
			m.histIdx = len(m.history)
			if line == "exit" || line == "quit" {
				return m, tea.Quit
			}
			out, quit := m.execute(line)
			if out != "" {
				m.appendOutput(out)
			}
			m.refreshViewport()
			if quit {
				return m, tea.Quit
			}
			return m, nil
		case tea.KeyUp:
			if len(m.history) > 0 && m.histIdx > 0 {
				m.histIdx--
				m.input.SetValue(m.history[m.histIdx])
			}
			return m, nil
		case tea.KeyDown:
			if m.histIdx < len(m.history)-1 {
				m.histIdx++
				m.input.SetValue(m.history[m.histIdx])
			} else {
				m.histIdx = len(m.history)
				m.input.SetValue("")
			}
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m builtinShellModel) View() string {
	var b strings.Builder
	b.WriteString("phaethon built-in shell")
	if m.reason != "" {
		b.WriteString("  [" + m.reason + "]")
	}
	b.WriteString("\n")
	b.WriteString(m.viewport.View())
	b.WriteString("\n")
	b.WriteString(m.cwd)
	b.WriteString("> ")
	b.WriteString(m.input.View())
	b.WriteString("\n")
	b.WriteString("Enter 执行 · ↑/↓ 历史 · Ctrl+C 退出 · help")
	return b.String()
}

func (m *builtinShellModel) appendOutput(s string) {
	if s == "" {
		return
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	m.output = append(m.output, lines...)
	const maxLines = 2000
	if len(m.output) > maxLines {
		m.output = m.output[len(m.output)-maxLines:]
	}
}

func (m *builtinShellModel) refreshViewport() {
	m.viewport.SetContent(strings.Join(m.output, "\n"))
	m.viewport.GotoBottom()
}

func (m *builtinShellModel) execute(line string) (string, bool) {
	fields := splitCommand(line)
	if len(fields) == 0 {
		return "", false
	}
	cmd := strings.ToLower(fields[0])
	args := fields[1:]

	switch cmd {
	case "help", "?":
		return strings.Join([]string{
			"内置命令:",
			"  help            显示帮助",
			"  pwd             当前目录",
			"  cd <dir>        切换目录（~ 为 home）",
			"  ls [dir]        列目录",
			"  cat <file>      查看文件（最多 64KB）",
			"  ps              进程列表",
			"  clear           清屏",
			"  !<命令>         调用系统 shell 执行",
			"  exit / quit     退出",
		}, "\n"), false
	case "pwd":
		return m.cwd, false
	case "cd":
		return m.changeDir(args), false
	case "ls":
		return m.listDir(args), false
	case "cat":
		return m.catFile(args), false
	case "ps":
		return m.runSystem("ps"), false
	case "clear":
		m.output = nil
		return "", false
	}
	if strings.HasPrefix(line, "!") {
		return m.runSystem(strings.TrimSpace(line[1:])), false
	}
	return "未知命令: " + cmd + "（输入 help 查看帮助）", false
}

func (m *builtinShellModel) changeDir(args []string) string {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	if target == "" || target == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			target = home
		}
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(m.cwd, target)
	}
	target = filepath.Clean(target)
	fi, err := os.Stat(target)
	if err != nil {
		return "cd: " + err.Error()
	}
	if !fi.IsDir() {
		return "cd: not a directory: " + target
	}
	m.cwd = target
	return ""
}

func (m *builtinShellModel) listDir(args []string) string {
	dir := m.cwd
	if len(args) > 0 {
		dir = args[0]
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(m.cwd, dir)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "ls: " + err.Error()
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, "  ")
}

func (m *builtinShellModel) catFile(args []string) string {
	if len(args) == 0 {
		return "cat: missing file"
	}
	path := args[0]
	if !filepath.IsAbs(path) {
		path = filepath.Join(m.cwd, path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "cat: " + err.Error()
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n, _ := f.Read(buf)
	return string(buf[:n])
}

func (m *builtinShellModel) runSystem(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	if command == "ps" {
		if runtime.GOOS == "windows" {
			command = "tasklist"
		} else {
			command = "ps aux"
		}
	}
	shell := autoDetectShell()
	c := exec.Command(shell, execArgs(shell, command)...)
	c.Dir = m.cwd
	c.Env = sessionEnv()
	out, err := c.CombinedOutput()
	if len(out) > 256<<10 {
		out = out[len(out)-(256<<10):]
	}
	text := string(out)
	if err != nil {
		text += "\n[" + err.Error() + "]"
	}
	return text
}

// splitCommand is a small quote-aware splitter for the built-in shell. It
// supports single/double quotes and backslash escaping; it is not a full shell
// parser (use !<command> for that).
func splitCommand(line string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble, escaped := false, false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range line {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && !inSingle:
			escaped = true
		case r == '\'' && !inDouble:
			inSingle = !inSingle
		case r == '"' && !inSingle:
			inDouble = !inDouble
		case (r == ' ' || r == '\t') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}
