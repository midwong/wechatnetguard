package main

import (
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hideWindow 返回隐藏子进程控制台窗口的 SysProcAttr
func hideWindow() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}

var _ = os.Stdout

// wechatRunning 通过进程快照判断微信相关进程是否正在运行。
// 使用 Windows API 而不是 tasklist 子进程，开销极小且无需隐藏窗口。
func wechatRunning() bool {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)

	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))

	if err := windows.Process32First(snap, &pe); err != nil {
		return false
	}
	for {
		name := windows.UTF16ToString(pe.ExeFile[:])
		for _, target := range wechatProcesses {
			if strings.EqualFold(name, target) {
				return true
			}
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			return false
		}
	}
}
