//go:build windows

package server

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"phaethon/util"
)

// winpty 0.4.3 provides a PTY for Windows versions that predate ConPTY
// (Win10 1809+). It is shipped next to the deployment binary:
//
//	<deploy>/winpty.dll
//	<deploy>/winpty-agent.exe
//
// winpty.dll is loaded dynamically (no cgo); winpty-agent.exe is the hidden
// console host process that winpty spawns for each session.
const (
	winptySpawnFlagAutoShutdown = 1
)

type winptyAPI struct {
	dll *syscall.LazyDLL

	open, configNew, configSetSize, configFree *syscall.LazyProc
	spawnConfigNew, spawnConfigFree, spawn     *syscall.LazyProc
	coninName, conoutName                      *syscall.LazyProc
	setSize, free                              *syscall.LazyProc
	errCode, errMsg, errFree                   *syscall.LazyProc
}

var (
	winptyLoadOnce  sync.Once
	winptyLoaded    bool
	winptyAPIv      *winptyAPI
	winptyProbeOnce sync.Once
	winptyProbeOK   bool
)

func fileExistsK(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// winptySearchDirs covers the direct deployment layout (exe next to winpty.*),
// the pkg/worker layout (data/worker/phaethon-<ver>, winpty in the deploy root
// two levels up), and the current working directory.
func winptySearchDirs() []string {
	var dirs []string
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		dirs = append(dirs, d, filepath.Join(d, ".."), filepath.Join(d, "..", ".."))
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	return dirs
}

func winptyLoad() bool {
	winptyLoadOnce.Do(func() {
		for _, dir := range winptySearchDirs() {
			dllPath := filepath.Join(dir, "winpty.dll")
			agentPath := filepath.Join(dir, "winpty-agent.exe")
			if !fileExistsK(dllPath) || !fileExistsK(agentPath) {
				continue
			}
			// winpty may look for the agent on PATH; make the deployment
			// directory visible before any session is created.
			_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			dll := syscall.NewLazyDLL(dllPath)
			if err := dll.Load(); err != nil {
				util.LogModuleWarn("ssh", "winpty: load %s failed: %v", dllPath, err)
				continue
			}
			winptyAPIv = &winptyAPI{
				dll:             dll,
				open:            dll.NewProc("winpty_open"),
				configNew:       dll.NewProc("winpty_config_new"),
				configSetSize:   dll.NewProc("winpty_config_set_initial_size"),
				configFree:      dll.NewProc("winpty_config_free"),
				spawnConfigNew:  dll.NewProc("winpty_spawn_config_new"),
				spawnConfigFree: dll.NewProc("winpty_spawn_config_free"),
				spawn:           dll.NewProc("winpty_spawn"),
				coninName:       dll.NewProc("winpty_conin_name"),
				conoutName:      dll.NewProc("winpty_conout_name"),
				setSize:         dll.NewProc("winpty_set_size"),
				free:            dll.NewProc("winpty_free"),
				errCode:         dll.NewProc("winpty_error_code"),
				errMsg:          dll.NewProc("winpty_error_msg"),
				errFree:         dll.NewProc("winpty_error_free"),
			}
			winptyLoaded = true
			util.LogModuleInfo("ssh", "winpty: loaded from %s", dir)
			return
		}
		util.LogModuleInfo("ssh", "winpty: not available (winpty.dll/winpty-agent.exe missing)")
	})
	return winptyLoaded
}

// winptyReady reports whether a real PTY can be created through winpty.
// It runs a one-time end-to-end probe (spawn a trivial cmd) so that a present
// but unusable winpty (AV/SRP blocked) makes pty-req fail and the client falls
// back to cooked pipe mode instead of getting a black-hole session.
func winptyReady() bool {
	if !winptyLoad() {
		return false
	}
	winptyProbeOnce.Do(func() {
		p, err := startWinptyProc("cmd.exe", "cmd.exe /c exit 0", "", 80, 25, false)
		if err != nil {
			util.LogModuleWarn("ssh", "winpty: probe failed: %v", err)
			return
		}
		p.CloseStdin()
		p.CloseIO()
		done := make(chan struct{})
		go func() {
			_ = p.Wait()
			close(done)
		}()
		select {
		case <-done:
			winptyProbeOK = true
			util.LogModuleInfo("ssh", "winpty: probe ok")
		case <-time.After(5 * time.Second):
			p.Kill()
			<-done
			util.LogModuleWarn("ssh", "winpty: probe timed out")
		}
	})
	return winptyProbeOK
}

var (
	wtsapi32                        = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQuerySessionInformationW = wtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory               = wtsapi32.NewProc("WTSFreeMemory")
)

// wtsSessionActive reports whether the worker's Windows session is an active
// desktop session. winpty renders output by scraping its hidden console's
// screen buffer; on Win7 a disconnected session (qwinsta: "Disc") still
// delivers input and runs commands, but conout only yields the initial clear,
// so SSH would look empty. Enabling winpty only when the session is active
// gives a real PTY when an administrator is connected and keeps the stable
// line-mode fallback when the node is unattended.
func wtsSessionActive() bool {
	const (
		wtsCurrentServerHandle = 0
		wtsCurrentSession      = 0xFFFFFFFF
		wtsConnectState        = 8 // WTS_INFO_CLASS::WTSConnectState
		wtsActive              = 0
	)
	var buf uintptr
	var n uint32
	r, _, _ := procWTSQuerySessionInformationW.Call(
		wtsCurrentServerHandle, wtsCurrentSession, wtsConnectState,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r == 0 || buf == 0 {
		return false
	}
	state := *(*uint32)(unsafe.Pointer(buf))
	procWTSFreeMemory.Call(buf)
	return state == wtsActive
}

func winptyWStr(proc *syscall.LazyProc, wp uintptr) string {
	r, _, _ := proc.Call(wp)
	if r == 0 {
		return ""
	}
	// winpty_conin_name/winpty_conout_name return LPCWSTR.
	return windows.UTF16PtrToString((*uint16)(unsafe.Pointer(r)))
}

func winptyTakeError(api *winptyAPI, errp uintptr) error {
	if errp == 0 {
		return nil
	}
	code := uint32(0)
	if r, _, _ := api.errCode.Call(errp); true {
		code = uint32(r)
	}
	msg := ""
	if r, _, _ := api.errMsg.Call(errp); r != 0 {
		msg = windows.UTF16PtrToString((*uint16)(unsafe.Pointer(r)))
	}
	api.errFree.Call(errp)
	if code == 0 && msg == "" {
		return nil
	}
	return fmt.Errorf("winpty error %d: %s", code, msg)
}

// startWinptyProc creates a winpty PTY, spawns appname/cmdline and returns a
// sessionProc backed by the PTY pipes.
func startWinptyProc(appname, cmdline, workDir string, cols, rows uint32, legacyExit bool) (*winptyProc, error) {
	api := winptyAPIv
	if api == nil {
		return nil, fmt.Errorf("winpty not loaded")
	}
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	var errp uintptr
	cfg, _, _ := api.configNew.Call(0, uintptr(unsafe.Pointer(&errp)))
	if err := winptyTakeError(api, errp); err != nil {
		return nil, err
	}
	if cfg == 0 {
		return nil, fmt.Errorf("winpty_config_new returned nil")
	}
	defer api.configFree.Call(cfg)
	api.configSetSize.Call(cfg, uintptr(cols), uintptr(rows))

	errp = 0
	wp, _, _ := api.open.Call(cfg, uintptr(unsafe.Pointer(&errp)))
	if err := winptyTakeError(api, errp); err != nil {
		return nil, err
	}
	if wp == 0 {
		return nil, fmt.Errorf("winpty_open returned nil")
	}
	fail := func(err error) (*winptyProc, error) {
		api.free.Call(wp)
		return nil, err
	}

	// CreateProcess with a non-NULL lpApplicationName does NOT search PATH:
	// resolve the executable to an absolute path, otherwise pass NULL and let
	// CreateProcess parse the (quoted) command line.
	appNamePtr := uintptr(0)
	if appname != "" {
		full := appname
		if !filepath.IsAbs(full) {
			if p, err := exec.LookPath(full); err == nil {
				full = p
			} else {
				full = ""
			}
		}
		if full != "" {
			if a16, err := syscall.UTF16PtrFromString(full); err == nil {
				appNamePtr = uintptr(unsafe.Pointer(a16))
			}
		}
	}
	cmd16, err := syscall.UTF16PtrFromString(cmdline)
	if err != nil {
		return fail(err)
	}
	var cwdPtr uintptr
	if workDir != "" {
		if cwd16, err := syscall.UTF16PtrFromString(workDir); err == nil {
			cwdPtr = uintptr(unsafe.Pointer(cwd16))
		}
	}

	errp = 0
	scfg, _, _ := api.spawnConfigNew.Call(
		uintptr(winptySpawnFlagAutoShutdown),
		appNamePtr,
		uintptr(unsafe.Pointer(cmd16)),
		cwdPtr,
		0,
		uintptr(unsafe.Pointer(&errp)))
	if err := winptyTakeError(api, errp); err != nil {
		return fail(err)
	}
	if scfg == 0 {
		return fail(fmt.Errorf("winpty_spawn_config_new returned nil"))
	}
	defer api.spawnConfigFree.Call(scfg)

	var procHandle, threadHandle uintptr
	var createErr uint32
	errp = 0
	ok, _, _ := api.spawn.Call(wp, scfg,
		uintptr(unsafe.Pointer(&procHandle)),
		uintptr(unsafe.Pointer(&threadHandle)),
		uintptr(unsafe.Pointer(&createErr)),
		uintptr(unsafe.Pointer(&errp)))
	if err := winptyTakeError(api, errp); err != nil {
		return fail(fmt.Errorf("%w (CreateProcess error %d)", err, createErr))
	}
	if ok == 0 {
		return fail(fmt.Errorf("winpty_spawn failed (CreateProcess error %d)", createErr))
	}
	if threadHandle != 0 {
		_ = windows.CloseHandle(windows.Handle(threadHandle))
	}

	inName := winptyWStr(api.coninName, wp)
	outName := winptyWStr(api.conoutName, wp)
	if inName == "" || outName == "" {
		_ = windows.TerminateProcess(windows.Handle(procHandle), 1)
		_ = windows.CloseHandle(windows.Handle(procHandle))
		return fail(fmt.Errorf("winpty pipe names are empty"))
	}
	// winpty creates conin as an inbound pipe (client writes) and conout as
	// an outbound pipe (client reads). Opening either with O_RDWR is rejected
	// with ERROR_ACCESS_DENIED, so use the matching direction.
	in, err := os.OpenFile(inName, os.O_WRONLY, 0)
	if err != nil {
		_ = windows.TerminateProcess(windows.Handle(procHandle), 1)
		_ = windows.CloseHandle(windows.Handle(procHandle))
		return fail(fmt.Errorf("open conin: %w", err))
	}
	out, err := os.OpenFile(outName, os.O_RDONLY, 0)
	if err != nil {
		_ = in.Close()
		_ = windows.TerminateProcess(windows.Handle(procHandle), 1)
		_ = windows.CloseHandle(windows.Handle(procHandle))
		return fail(fmt.Errorf("open conout: %w", err))
	}
	return &winptyProc{
		api:    api,
		wp:     wp,
		in:     in,
		out:    out,
		proc:   windows.Handle(procHandle),
		legacy: legacyExit,
	}, nil
}

type winptyProc struct {
	api  *winptyAPI
	wp   uintptr
	in   *os.File
	out  *os.File
	proc windows.Handle

	mu    sync.Mutex
	freed bool

	// legacy marks Win7/PS2 shells. PS2's interactive host does not exit on
	// "exit" under winpty and does not exit when conin closes, so the server
	// terminates the process itself after forwarding "exit" or on EOF.
	legacy  bool
	lineBuf []byte
}

func (p *winptyProc) Read(b []byte) (int, error) { return p.out.Read(b) }
func (p *winptyProc) Write(b []byte) (int, error) {
	n, err := p.in.Write(b)
	if p.legacy {
		p.scanExitLine(b)
	}
	return n, err
}

// scanExitLine watches for a line equal to "exit" on legacy (Win7/PS2)
// sessions and terminates the shell shortly afterwards: PS2 under winpty does
// not exit on its own, which would leave the SSH session hanging.
func (p *winptyProc) scanExitLine(b []byte) {
	p.lineBuf = append(p.lineBuf, b...)
	for {
		i := bytes.IndexAny(p.lineBuf, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(p.lineBuf[:i]))
		p.lineBuf = p.lineBuf[i+1:]
		if strings.EqualFold(line, "exit") {
			time.AfterFunc(900*time.Millisecond, p.killIfAlive)
			return
		}
	}
	if len(p.lineBuf) > 4096 {
		p.lineBuf = p.lineBuf[len(p.lineBuf)-4096:]
	}
}

func (p *winptyProc) killIfAlive() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.freed || p.proc == 0 {
		return
	}
	_ = windows.TerminateProcess(p.proc, 0)
}

func (p *winptyProc) CloseStdin() {
	_ = p.in.Close()
}

func (p *winptyProc) Wait() error {
	_, _ = windows.WaitForSingleObject(p.proc, windows.INFINITE)
	var code uint32
	_ = windows.GetExitCodeProcess(p.proc, &code)
	p.release()
	if code != 0 {
		return &exitCodeError{code: code}
	}
	return nil
}

func (p *winptyProc) Kill() {
	if p.proc != 0 {
		_ = windows.TerminateProcess(p.proc, 1)
	}
}

func (p *winptyProc) CloseIO() { _ = p.out.Close() }

func (p *winptyProc) Resize(cols, rows uint32) {
	if cols == 0 || rows == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.freed {
		return
	}
	var errp uintptr
	p.api.setSize.Call(p.wp, uintptr(cols), uintptr(rows), uintptr(unsafe.Pointer(&errp)))
	_ = winptyTakeError(p.api, errp)
}

func (p *winptyProc) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.freed {
		return
	}
	p.freed = true
	p.api.free.Call(p.wp)
	if p.proc != 0 {
		_ = windows.CloseHandle(p.proc)
		p.proc = 0
	}
}
