// wechatguard: Windows 服务。监测微信是否运行；微信运行中若断网超过 20 秒，
// 强制退出微信，防止断网状态下微信窗口中的内容被别人查看。
//
// 用法:
//   wechatguard.exe install    安装并启动 Windows 服务（需要管理员权限）
//   wechatguard.exe uninstall  停止并卸载服务（需要管理员权限）
//   wechatguard.exe start      启动服务（需要管理员权限）
//   wechatguard.exe stop       停止服务（需要管理员权限）
//   wechatguard.exe test       单次检测网络连通性（控制台调试用）
//   wechatguard.exe console    在控制台前台运行监测逻辑（调试用）
//   wechatguard.exe            （由 SCM 启动时）作为 Windows 服务运行
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	svcName        = "WeChatNetGuard"
	svcDisplayName = "WeChat Network Guard"
	svcDescription = "监测微信运行状态：微信运行中断网 20 秒后强制退出微信，防止微信内容被他人查看。微信未运行时仅低频待机。"

	// version 由 -ldflags "-X main.version=xxx" 注入
	version = "dev"

	// 断网后延迟退出微信的时长
	offlineGracePeriod = 20 * time.Second
	// 微信运行时，每隔多久检测一次网络
	checkInterval = 5 * time.Second
	// 观察期内每隔多久复查一次
	graceCheckInterval = 2 * time.Second
	// 微信未运行时，每隔多久看一眼微信进程是否存在
	idleInterval = 10 * time.Second
	// 退出微信后的冷却期，避免反复操作
	killCooldown = 5 * time.Minute
)

// 需要结束的微信相关进程
var wechatProcesses = []string{"WeChat.exe", "Weixin.exe", "WeChatAppEx.exe"}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)

	inService, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("无法判断服务模式: %v", err)
	}
	if inService {
		runAsService()
		return
	}

	if len(os.Args) < 2 {
		usage()
		return
	}
	cmd := strings.ToLower(os.Args[1])
	switch cmd {
	case "install":
		installService()
	case "uninstall":
		uninstallService()
	case "start":
		controlService(func(s *mgr.Service) error { return s.Start() })
	case "stop":
		controlService(func(s *mgr.Service) error {
			st, err := s.Query()
			if err != nil {
				return err
			}
			if _, err := s.Control(svc.Stop); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			return waitStopped(s, st, ctx)
		})
	case "test":
		runTest()
	case "console":
		runConsole()
	default:
		usage()
	}
}

func usage() {
	fmt.Printf(`WeChat Network Guard (wechatguard) %s - 断网后延迟退出微信的保护服务

用法: wechatguard.exe <命令>`, version)
	fmt.Println(`

用法: wechatguard.exe <命令>
  install    安装并启动 Windows 服务（需要管理员权限运行）
  uninstall  停止并卸载 Windows 服务（需要管理员权限运行）
  start      启动服务
  stop       停止服务
  test       单次检测网络连通性并显示检测结果
  console    在控制台前台运行监测逻辑（调试用）`)
}

// ---------------------------------------------------------------- 服务模式

func runAsService() {
	// 服务运行在 SYSTEM 账户下，日志写到 ProgramData，任何管理员都能查看
	logFile, err := setupFileLog()
	if err == nil {
		defer logFile.Close()
		log.Printf("服务启动，可执行文件: %s", exePath())
	}

	err = svc.Run(svcName, &guardService{})
	if err != nil {
		log.Printf("svc.Run 失败: %v", err)
	}
	log.Printf("服务退出: %v", err)
}

func setupFileLog() (*os.File, error) {
	dir := filepath.Join(os.Getenv("ProgramData"), "WeChatNetGuard")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "wechatnetguard.log")

	// 简单的日志滚动：超过 5MB 就重新开始
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		_ = os.Remove(path + ".old")
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	log.SetOutput(f)
	return f, nil
}

type guardService struct{}

func (m *guardService) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepts = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		monitorLoop(ctx)
		close(done)
	}()

	status <- svc.Status{State: svc.Running, Accepts: accepts}
loop:
	for c := range req {
		switch c.Cmd {
		case svc.Interrogate:
			status <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			break loop
		}
	}
	status <- svc.Status{State: svc.StopPending}
	cancel()
	<-done
	return false, 0
}

// ---------------------------------------------------------------- 监测逻辑

// 网络连通性检测：同时探测多家公共 DNS 的 TCP 端口，任一个能连上即认为在线
var checkEndpoints = []string{
	"223.5.5.5:443",     // 阿里 DNS DoT
	"119.29.29.29:443",  // 腾讯 DNS DoH
	"114.114.114.114:53",
	"1.1.1.1:443",
}

func isOnline() bool {
	// 并发探测所有端点，任一成功即在线；整体 3 秒封顶，避免串行超时拖慢检测节奏
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	results := make(chan bool, len(checkEndpoints))
	dialer := &net.Dialer{}
	for _, addr := range checkEndpoints {
		go func(addr string) {
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			if err == nil {
				_ = conn.Close()
				results <- true
			} else {
				results <- false
			}
		}(addr)
	}
	for i := 0; i < len(checkEndpoints); i++ {
		if <-results {
			return true
		}
	}
	return false
}

// 监测主循环。
// 微信未运行时进入待机：仅每 idleInterval(10s) 检查一次微信是否启动，不做网络探测。
// 微信运行中才监测网络：每 checkInterval 检测一次；检测为离线后进入 20 秒观察期，
// 观察期内每 graceCheckInterval 复查一次，期间恢复在线则取消；否则结束微信。
func monitorLoop(ctx context.Context) {
	log.Printf("监测循环开始: 微信未运行时每 %v 检查一次微信进程；微信运行中断网延迟 %v 后退出微信（每 %v 检测网络）",
		idleInterval, offlineGracePeriod, checkInterval)

	var lastKillTime time.Time
	var lastRunning bool

	for {
		if ctx.Err() != nil {
			return
		}

		// ---------- 微信未运行 → 待机，只盯是否登录微信 ----------
		if !wechatRunning() {
			if lastRunning {
				log.Printf("微信已不在运行，进入待机状态")
				lastRunning = false
			}
			if !sleepCtx(ctx, idleInterval) {
				return
			}
			continue
		}
		if !lastRunning {
			log.Printf("检测到微信正在运行，开始监测网络")
			lastRunning = true
		}

		// ---------- 微信运行中 → 网络监测 ----------
		if isOnline() {
			if !sleepCtx(ctx, checkInterval) {
				return
			}
			continue
		}

		offlineAt := time.Now()
		log.Printf("检测到断网，进入 %v 观察期...", offlineGracePeriod)

		stillOffline, stopped := stayOfflineForGrace(ctx)
		if stopped {
			return
		}
		if !stillOffline {
			continue // 观察期内网络恢复，回到正常监测
		}

		if wechatRunning() {
			// 观察期内没有恢复网络且微信仍在运行 → 退出微信
			elapsed := time.Since(offlineAt).Truncate(time.Second)
			if time.Since(lastKillTime) < killCooldown {
				log.Printf("持续断网 %v，但仍处于 %v 冷却期内，本次不操作", elapsed, killCooldown)
				if !sleepCtx(ctx, idleInterval) {
					return
				}
				continue
			}
			lastKillTime = time.Now()
			log.Printf("持续断网 %v，强制退出微信...", elapsed)
			killWeChat()
		} else {
			log.Printf("断网观察期结束，微信已不在运行，无需处理")
		}
		// 回到循环开头：killWeChat 后微信不在了会自然进入待机状态
	}
}

// stayOfflineForGrace: 观察期内每 graceCheckInterval 复查一次网络。
// 返回值: stillOffline=观察期满仍未恢复网络；stopped=收到服务停止信号。
func stayOfflineForGrace(ctx context.Context) (stillOffline, stopped bool) {
	deadline := time.Now().Add(offlineGracePeriod)
	for time.Now().Before(deadline) {
		if !sleepCtx(ctx, graceCheckInterval) {
			return false, true
		}
		if isOnline() {
			log.Printf("观察期内网络恢复，取消退出微信")
			return false, false
		}
	}
	return true, false
}

// sleepCtx: 可被 ctx 取消的 sleep；返回 false 表示 ctx 已取消
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func killWeChat() {
	for _, name := range wechatProcesses {
		// /F 强制结束，/T 结束进程树
		cmd := exec.Command("taskkill", "/F", "/T", "/IM", name)
		cmd.SysProcAttr = hideWindow()
		out, err := cmd.CombinedOutput()
		outStr := strings.TrimSpace(string(out))
		if err != nil {
			// 进程不存在时的正常情况，不作为错误记录
			if strings.Contains(outStr, "not found") || strings.Contains(outStr, "没有找到进程") ||
				strings.Contains(outStr, "找不到") {
				log.Printf("未发现 %s 进程，跳过", name)
			} else {
				log.Printf("结束 %s 失败: %v, 输出: %s", name, err, outStr)
			}
			continue
		}
		log.Printf("已结束 %s: %s", name, outStr)
	}
}

// ---------------------------------------------------------------- 服务安装

func installService() {
	exe := exePath()

	m, err := mgr.Connect()
	if err != nil {
		fmt.Printf("连接服务管理器失败(需要管理员权限吗?): %v\n", err)
		os.Exit(1)
	}
	defer m.Disconnect()

	// 若已存在同名服务，先删除旧版本
	if s, err := m.OpenService(svcName); err == nil {
		fmt.Printf("发现已存在的 %s 服务，先卸载...\n", svcName)
		_ = tryStopService(s)
		_ = s.Delete()
		s.Close()
		time.Sleep(2 * time.Second)
	}

	cfg := mgr.Config{
		DisplayName: svcDisplayName,
		Description: svcDescription,
		StartType:   mgr.StartAutomatic, // 开机自动运行
	}
	s, err := m.CreateService(svcName, exe, cfg)
	if err != nil {
		fmt.Printf("创建服务失败: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	// 服务异常退出时自动重启
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
	}, 24*3600); err != nil {
		fmt.Printf("设置失败自动重启策略失败(可忽略): %v\n", err)
	}

	fmt.Printf("服务 %s 安装成功\n", svcName)

	if err := s.Start(); err != nil {
		fmt.Printf("服务已安装但启动失败: %v (可稍后手动执行 wechatguard.exe start)\n", err)
		return
	}
	fmt.Println("服务已启动。日志位于 %ProgramData%\\WeChatNetGuard\\wechatnetguard.log")
}

func uninstallService() {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Printf("连接服务管理器失败(需要管理员权限吗?): %v\n", err)
		os.Exit(1)
	}
	defer m.Disconnect()

	s, err := m.OpenService(svcName)
	if err != nil {
		fmt.Printf("服务 %s 不存在: %v\n", svcName, err)
		return
	}
	defer s.Close()

	if err := tryStopService(s); err == nil {
		fmt.Println("服务已停止")
	} else {
		fmt.Printf("停止服务时跳过/失败: %v\n", err)
	}

	if err := s.Delete(); err != nil {
		fmt.Printf("删除服务失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("服务 %s 已卸载\n", svcName)
}

func controlService(action func(*mgr.Service) error) {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Printf("连接服务管理器失败(需要管理员权限吗?): %v\n", err)
		os.Exit(1)
	}
	defer m.Disconnect()

	s, err := m.OpenService(svcName)
	if err != nil {
		fmt.Printf("打开服务 %s 失败: %v\n", svcName, err)
		os.Exit(1)
	}
	defer s.Close()

	if err := action(s); err != nil {
		fmt.Printf("操作失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("操作完成")
}

// tryStopService 供 install(覆盖旧服务)、uninstall 复用
func tryStopService(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.Control(svc.Stop); err != nil {
		return err
	}
	return waitStopped(s, st, ctx)
}

func waitStopped(s *mgr.Service, prev svc.Status, ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待服务停止超时")
		case <-time.Tick(300 * time.Millisecond):
			st, err := s.Query()
			if err != nil {
				return err
			}
			if st.State == svc.Stopped {
				return nil
			}
			prev = st
		}
	}
}

// ---------------------------------------------------------------- 调试命令

func runTest() {
	fmt.Print("正在检测网络连通性(所有端点并发探测，整体最多 3 秒)...\n\n")
	online := false
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dialer := &net.Dialer{}
	type probeResult struct {
		addr    string
		elapsed time.Duration
		err     error
	}
	results := make(chan probeResult, len(checkEndpoints))
	for _, addr := range checkEndpoints {
		go func(addr string) {
			start := time.Now()
			conn, err := dialer.DialContext(ctx, "tcp", addr)
			elapsed := time.Since(start)
			if err == nil {
				_ = conn.Close()
			}
			results <- probeResult{addr, elapsed, err}
		}(addr)
	}
	for i := 0; i < len(checkEndpoints); i++ {
		r := <-results
		if r.err == nil {
			online = true
			fmt.Printf("  [在线] %s  (%v)\n", r.addr, r.elapsed.Truncate(time.Millisecond))
		} else {
			fmt.Printf("  [失败] %s  (%v) %v\n", r.addr, r.elapsed.Truncate(time.Millisecond), r.err)
		}
	}
	fmt.Println()
	fmt.Printf("网络状态: %s\n", map[bool]string{true: "在线", false: "断开！服务运行状态下会在 20 秒后退出微信"}[online])
	fmt.Printf("微信状态: %s\n", map[bool]string{true: "正在运行(服务将监测网络)", false: "未运行(服务仅每 10s 待机检查)"}[wechatRunning()])
}

func runConsole() {
	log.SetFlags(log.Ldate | log.Ltime)
	fmt.Println("控制台调试模式运行(按 Ctrl+C 退出)。日志输出到屏幕:")
	// 同时输出到屏幕
	log.SetOutput(os.Stdout)
	fmt.Printf("配置: 微信未运行待机间隔 %v / 微信运行时网络检测间隔 %v / 断网延迟 %v / 冷却期 %v\n\n",
		idleInterval, checkInterval, offlineGracePeriod, killCooldown)
	monitorLoop(context.Background())
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
