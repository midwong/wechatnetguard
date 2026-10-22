# WeChat Network Guard (wechatguard)

一个极小的 Windows 服务程序：**监测网络断开，延迟 20 秒强制退出微信**，防止断网状态下电脑上的微信窗口内容被他人查看。

如果微信正开着，断网 20 秒后微信被自动退出（比如你拔掉网线临时离开电脑）；微信没打开时服务完全待机。

![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)
![Platform](https://img.shields.io/badge/Platform-Windows-blue)
![License](https://img.shields.io/badge/License-MIT-green)

## 功能

- 以 **Windows 服务**（`WeChatNetGuard`，SYSTEM 账户）运行，开机自动启动，无需桌面登录
- **微信未运行时完全待机**：每 10 秒检查一次微信进程是否启动，不做任何网络探测
- **微信运行时监测网络**：每 5 秒并发探测 4 个公共 DNS 端点（阿里/腾讯/114DNS/Cloudflare），任一可达即在线，误报率低
- 判定断网后进入 **20 秒观察期**（每 2 秒复查），短暂网络抖动自动取消，不误杀
- 观察期内仍断网 → `taskkill /F /T` 强制结束 `WeChat.exe` / `Weixin.exe` / `WeChatAppEx.exe`
- 结束后有 **5 分钟冷却期**，不会反复操作
- 服务异常退出自动重启（SCM Recovery 配置）
- 日志写入 `%ProgramData%\WeChatNetGuard\wechatnetguard.log`（超过 5MB 自动轮转）

## 快速开始（用户）

1. 从 [Releases](../../releases) 下载 `wechatguard-v*-windows-amd64.zip`，解压出 `wechatguard.exe`
2. **以管理员身份**打开 PowerShell/CMD：

```powershell
cd <解压目录>
.\wechatguard.exe install     # 安装并启动服务（开机自启）
```

3. 验证：

```powershell
.\wechatguard.exe test        # 查看网络/微信进程检测结果

# 查看服务日志
Get-Content "$env:ProgramData\WeChatNetGuard\wechatnetguard.log" -Encoding utf8
```

### 卸载 / 控制服务

```powershell
.\wechatguard.exe stop        # 停止服务
.\wechatguard.exe uninstall   # 停止并卸载服务
.\wechatguard.exe start       # 重新启动服务
```

### 暂停使用

```powershell
Stop-Service WeChatNetGuard      # 临时停止（管理员）
Start-Service WeChatNetGuard     # 重新启动
.\wechatguard.exe uninstall      # 彻底卸载，不再开机自启
```

## 命令一览

| 命令 | 说明 | 权限 |
|---|---|---|
| `install` | 安装并启动 Windows 服务 | 管理员 |
| `uninstall` | 停止并卸载服务 | 管理员 |
| `start` / `stop` | 启动 / 停止服务 | 管理员 |
| `test` | 单次检测网络与微信进程状态并显示 | 普通用户 |
| `console` | 在控制台前台运行监测逻辑（调试用） | 普通用户 |
| （无参数且由 SCM 启动） | 作为 Windows 服务运行 | SYSTEM |

## 从源码构建

```powershell
git clone https://github.com/<你的用户名>/wechatguard.git
cd wechatguard
go build -trimpath -ldflags "-s -w -H=windowsgui -X main.version=v1.0.0" -o wechatguard.exe .
```

要求：Go 1.22+（还没有 Go？[golang.org/dl](https://golang.org/dl)），无其他构建时网络依赖（`golang.org/x/sys` 已 vendor 或走代理）。

推送任意 `v*` 格式 tag（如 `v1.0.1`）时，GitHub Actions 会自动构建并发布 Release（见 `.github/workflows/release.yml`）。

## 工作原理

```
服务启动
   │
   ▼
(每 10s) 微信进程在运行吗？ ──否──► 保持待机（不做网络探测）
   │ 是
   ▼
(每 5s) 网络连通吗？ ──是──► 回到循环
   │ 否（进入 20s 观察期，每 2s 复查）
   │
   ├──期间网络恢复──► 取消，回到循环
   ▼
微信还在运行吗？ ──否──► 无需处理，回到循环
   │ 是
   ▼
在 5 分钟冷却期内吗？ ──是──► 跳过本次
   │ 否
   ▼
taskkill /F /T 退出微信 ──► 回到循环
```

## 注意事项 / FAQ

- **为什么强制 `taskkill /F`？** 微信没有公开可靠的"退出托盘程序"接口，温和关闭（WM_CLOSE）只会把窗口最小化到托盘，起不到退出效果。
- **服务运行需要什么样的权限？** 服务以 `SYSTEM` 账户运行，开机即生效；`install`/`uninstall` 等控制台操作需要管理员权限，`test`/`console` 普通用户即可。
- **会被杀毒软件误报吗？** 程序会枚举进程并结束微信进程，个别杀软可能弹提示。属于正常保护行为，加入白名单即可。
- **能改"20 秒"吗？** 可以，修改 `main.go` 顶部常量 `offlineGracePeriod` 后自行编译；也欢迎提 issue 建议支持配置文件。

## 许可证

[MIT](LICENSE)
