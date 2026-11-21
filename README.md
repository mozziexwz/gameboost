# GAMEBOOST

GAMEBOOST 是基于 [node_launcher](https://github.com/mozziexwz/node_launcher) 的 Windows 游戏加速启动器，支持按进程或目标 IP 分流。

## 使用

1. 在 64 位 Windows 10 或 11 上运行 `GAMEBOOST.exe`，按提示授予管理员权限以加载 WinDivert。
2. 在「节点配置」中导入 mieru JSON 文件，或点击「导入链接」粘贴 `mierus://` 分享链接。
3. 在「分流规则」选模式并保存清单。进程模式支持进程名、可执行文件完整路径和文件夹。添加文件夹会匹配该目录及其子目录中的程序。IP 模式填写目标 IPv4、IPv6 或 CIDR 网段，按连接的实际目的地址匹配。
4. 在「加速」选择节点并开始加速，再打开对应程序。底部状态栏显示累计节点流量和实时上下行速率。
5. 在「出口检测」中查看出口公网 IPv4、节点入口延迟，以及经当前代理访问 `https://ip.sb/` 的 HTTPS 响应延迟。程序发起两次请求，取较快的响应。

节点配置和设置保存在可执行文件旁的 `configs/` 和 `settings.json`。分享链接及保存后的节点配置可能包含节点凭据。

### DNS 防泄露

启动加速后，WinDivert 接管本机 IPv4 和 IPv6 的 UDP/TCP 53 查询，使用当前节点访问 Cloudflare DoH。Windows 当前配置的 DNS 服务器和常见加密解析器的 TCP DoH，以及 TCP 853 上的 DoT，经节点中继。加速期间会阻止这些解析器的 QUIC 回落及局域网名称广播。节点断开时，受保护的 DNS 请求失败，停止加速会释放临时过滤器。

进程模式也分流名单内应用自行发出的 HTTPS。IP 模式按连接地址匹配，应用自带且位于 DNS 保护范围及目标清单之外的 HTTPS 连接仍按 IP 规则处理。节点域名的启动解析使用固定地址上的 HTTPS。

## 构建

需要 Go 1.27.1、Node.js 24、Wails CLI v2.16.0 和包含 C++ 桌面开发组件的 Visual Studio 2022 Build Tools。

```powershell
git submodule update --init
.\scripts\fetch-windivert.ps1
.\scripts\build-core.ps1
go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0
go test ./...
wails build -clean -m -platform windows/amd64 -tags bundle
```

便携版输出到 `build/bin/GAMEBOOST.exe`，内置 ProxyBridge 4.1.0 和 WinDivert 2.2.2-A。仓库包含上游的开发记录和用于测试的 `cmd/msboostctl`。

在管理员 PowerShell 中运行 `./scripts/e2e-windows.ps1 -Outbound`，使用本机测试节点验证 WinDivert 分流和 DNS。验收日志写入 `.cache/e2e/windows-e2e-outbound.log`。

## 许可

项目使用 [GPL-3.0](LICENSE)。第三方组件的版权与许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。
