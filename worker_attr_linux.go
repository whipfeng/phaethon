//go:build linux

package main

import (
	"runtime"

	"golang.org/x/sys/unix"

	"phaethon/util"
)

// enableParentDeathSignal 让本进程（worker）在父进程（watchdog）死亡时收到 SIGTERM，
// 避免 watchdog 被 kill -9 后留下抢端口/抢 DB 的孤儿 worker。
//
// 为什么不用 SysProcAttr.Pdeathsig：该选项是**线程级**的——信号绑定在 fork 出子进程的
// 那个 OS 线程上，Go runtime 回收该线程时会把 worker 误杀。改在 worker 侧设置，
// 并由 LockOSThread 把主 goroutine 钉在一条长生命周期线程上，语义稳定。
//
// 返回的错误仅记日志，不影响启动。
func enableParentDeathSignal() {
	runtime.LockOSThread() // 故意不 Unlock：主 goroutine 常驻，线程不会被回收
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
		util.LogModuleWarn("main", "worker: set parent-death signal failed: %v", err)
		return
	}
	util.LogModuleInfo("main", "worker: parent-death signal armed (SIGTERM on watchdog exit)")
}
