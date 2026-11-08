//go:build !linux

package main

// enableParentDeathSignal 在非 Linux 平台无 PR_SET_PDEATHSIG 等价物，空实现。
// （Windows 侧由 watchdog 自己的 Job/进程管理负责，当前不在此处理。）
func enableParentDeathSignal() {}
