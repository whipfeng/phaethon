package server

import "io"

// sessionProc 抽象被拉起进程的双向管道与生命周期。
// 具体实现在 ssh_session_unix.go（pty/管道）与 ssh_session_windows.go（管道）。
type sessionProc interface {
	io.Reader
	io.Writer
	Wait() error
	Kill()
	CloseStdin()
	Resize(cols, rows uint32)
}
