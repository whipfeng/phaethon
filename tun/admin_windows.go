//go:build windows

package tun

import (
	"fmt"
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"phaethon/util"
)

// IsAdmin checks whether the current process has administrator privileges.
func IsAdmin() bool {
	var sid *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY,
		2,
		windows.SECURITY_BUILTIN_DOMAIN_RID,
		windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0,
		&sid,
	)
	if err != nil {
		return false
	}
	defer windows.FreeSid(sid)

	token := windows.Token(0)
	member, err := token.IsMember(sid)
	if err != nil {
		return false
	}
	return member
}

// RestartAsAdmin re-launches the current executable with UAC elevation
// and then exits the current process.
func RestartAsAdmin() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("get executable path: %w", err)
	}

	// Build argument string with proper escaping.
	// Windows command-line parsing treats " and \ specially.
	var args []string
	for _, a := range os.Args[1:] {
		args = append(args, windowsCmdEscape(a))
	}
	argStr := strings.Join(args, " ")

	util.LogInfo("tun: requesting administrator privileges via UAC...")

	verb, _ := windows.UTF16PtrFromString("runas")
	exePtr, _ := windows.UTF16PtrFromString(exe)
	var argPtr *uint16
	if argStr != "" {
		argPtr, _ = windows.UTF16PtrFromString(argStr)
	}

	shell32 := windows.NewLazySystemDLL("shell32.dll")
	shellExecute := shell32.NewProc("ShellExecuteW")

	ret, _, _ := shellExecute.Call(
		0,
		uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(exePtr)),
		uintptr(unsafe.Pointer(argPtr)),
		0,
		windows.SW_NORMAL,
	)
	if ret <= 32 {
		return fmt.Errorf("ShellExecute failed, code=%d", ret)
	}

	// Exit current process so only the elevated instance remains
	os.Exit(0)
	return nil
}

// EnsureAdminPrivileges checks for admin rights and attempts auto-elevation on Windows.
func EnsureAdminPrivileges() error {
	if IsAdmin() {
		return nil
	}
	// Not admin — try to relaunch with UAC
	if err := RestartAsAdmin(); err != nil {
		return fmt.Errorf("tun mode requires administrator privileges; auto-elevation failed: %w", err)
	}
	return nil
}

// windowsCmdEscape escapes an argument for Windows command-line parsing.
// If the argument contains spaces or quotes, it is wrapped in double quotes
// and internal backslash-quote sequences are properly escaped.
func windowsCmdEscape(arg string) string {
	needsQuoting := false
	for _, r := range arg {
		if r == ' ' || r == '\t' || r == '"' {
			needsQuoting = true
			break
		}
	}
	if !needsQuoting {
		return arg
	}
	// Count backslashes before each double quote and double them
	var b strings.Builder
	b.WriteByte('"')
	backslashes := 0
	for _, r := range arg {
		if r == '\\' {
			backslashes++
		} else if r == '"' {
			for i := 0; i < backslashes; i++ {
				b.WriteString("\\\\")
			}
			backslashes = 0
			b.WriteString("\\\"")
		} else {
			for i := 0; i < backslashes; i++ {
				b.WriteByte('\\')
			}
			backslashes = 0
			b.WriteRune(r)
		}
	}
	for i := 0; i < backslashes; i++ {
		b.WriteString("\\\\")
	}
	b.WriteByte('"')
	return b.String()
}
