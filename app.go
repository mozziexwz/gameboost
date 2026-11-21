package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/mozziexwz/gameboost/internal/app"
	"github.com/mozziexwz/gameboost/internal/engine"
	"github.com/mozziexwz/gameboost/internal/logx"
	"github.com/mozziexwz/gameboost/internal/redirector"
	"github.com/mozziexwz/gameboost/internal/store"
	"github.com/mozziexwz/gameboost/internal/winsys"
)

// coreInstance is shared with msboostctl so only one program drives the core.
// Share the core lock with node_launcher, since both reserve the relay ports.
const coreInstance = "MSBOOST-Launcher-Core"

// App is bound to the frontend; its exported methods are the UI's API.
type App struct {
	ctx      context.Context
	layout   store.Layout
	log      *logx.Logger
	ctl      *app.Controller
	exePath  string
	tray     *tray
	quitting atomic.Bool
	stopBg   context.CancelFunc

	releaseLock func()
	lockErr     error
	settingsErr string
	runtime     runtimeSource
}

func NewApp(layout store.Layout, log *logx.Logger, rt runtimeSource, exePath string) *App {
	a := &App{layout: layout, log: log, exePath: exePath, runtime: rt,
		ctl: app.NewWindows(layout, log, rt.corePath, rt.prepare)}
	if _, err := layout.LoadSettings(); err != nil {
		a.settingsErr = err.Error()
	}
	return a
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.releaseLock, a.lockErr = winsys.AcquireInstance(coreInstance)
	if a.lockErr != nil {
		a.log.Printf(logx.App, "分流核心正在被另一个程序使用，本窗口暂不能开始加速")
	}
	a.tray = startTray(trayIcons{idle: trayIdleIcon, running: trayRunningIcon},
		trayActions{show: a.ShowWindow, toggle: a.toggleFromTray, quit: a.quitFromTray})
	a.ctl.Watch(func(st app.Status) {
		runtime.EventsEmit(ctx, "status", toStatusDTO(st))
		a.tray.setState(st.State)
	})

	bg, cancel := context.WithCancel(ctx)
	a.stopBg = cancel
	go a.pushStatus(bg)
	go a.pushLogs(bg)
	a.log.Printf(logx.App, "GAMEBOOST 已启动，版本 %s，数据目录 %s", versionString(), a.layout.Root)
	a.log.Printf(logx.App, "管理员权限：%v", winsys.IsElevated())
	a.log.Printf(logx.App, "分流组件：%s", a.runtime.describe)
}

// pushStatus refreshes the traffic figures once a second while accelerating.
func (a *App) pushStatus(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if st := a.ctl.Status(); st.State == app.StateRunning {
				runtime.EventsEmit(a.ctx, "status", toStatusDTO(st))
			}
		}
	}
}

// pushLogs forwards new log lines in small batches.
func (a *App) pushLogs(ctx context.Context) {
	lines, cancel := a.log.Subscribe()
	defer cancel()
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var batch []LogLineDTO
	for {
		select {
		case <-ctx.Done():
			return
		case l, ok := <-lines:
			if !ok {
				return
			}
			batch = append(batch, toLogLineDTO(l))
		case <-t.C:
			if len(batch) > 0 {
				runtime.EventsEmit(a.ctx, "logs", batch)
				batch = nil
			}
		}
	}
}

// beforeClose hides the window to the tray; only an explicit quit exits.
func (a *App) beforeClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return false
	}
	runtime.WindowHide(ctx)
	return true
}

func (a *App) shutdown(context.Context) {
	a.ctl.Shutdown()
	if a.stopBg != nil {
		a.stopBg()
	}
	a.tray.stop()
	if a.releaseLock != nil {
		a.releaseLock()
	}
	a.log.Printf(logx.App, "GAMEBOOST 已退出")
	a.log.Close()
}

func (a *App) onSecondInstance(options.SecondInstanceData) {
	a.ShowWindow()
}

// ---- tray ----

func (a *App) toggleFromTray() {
	switch a.ctl.Status().State {
	case app.StateRunning:
		running := a.ctl.RunningProcesses()
		if a.ctl.Settings().Mode == store.ModeIP || len(running) > 0 {
			message := "停止加速会断开代理连接。确定停止吗？"
			if len(running) > 0 {
				message = "正在运行 " + strings.Join(running, "、") + "。" + message
			}
			if !a.confirm("停止加速", message) {
				return
			}
		}
		if err := a.Stop(); err != nil {
			a.notify("error", "停止加速失败："+err.Error())
		}
	case app.StateIdle:
		warnings, err := a.Start()
		if err != nil {
			a.notify("error", "开始加速失败："+err.Error())
		}
		for _, w := range warnings {
			a.notify("warn", w)
		}
	}
}

func (a *App) quitFromTray() {
	if a.ctl.Status().State != app.StateIdle && !a.confirm("退出", "退出会停止加速并断开代理连接。确定退出吗？") {
		return
	}
	a.Quit()
}

// confirm asks a yes/no question with a native dialog.
func (a *App) confirm(title, message string) bool {
	answer, err := runtime.MessageDialog(a.ctx, runtime.MessageDialogOptions{
		Type: runtime.QuestionDialog, Title: title, Message: message,
	})
	return err == nil && (answer == "Yes" || answer == "是")
}

// NoticeDTO is a message for the main window from a tray action.
type NoticeDTO struct {
	Kind string `json:"kind"` // error | warn | info
	Text string `json:"text"`
}

// notify brings the window forward and shows a message in the UI.
func (a *App) notify(kind, text string) {
	a.ShowWindow()
	runtime.EventsEmit(a.ctx, "notice", NoticeDTO{Kind: kind, Text: text})
}

// ---- API used by the frontend ----

// Overview returns everything the pages need in one call.
func (a *App) Overview() OverviewDTO {
	settings := a.ctl.Settings()
	list, _ := a.layout.ListConfigs()
	elevated, conflicts := a.ctl.Preflight()
	dto := OverviewDTO{
		Status:         toStatusDTO(a.ctl.Status()),
		Configs:        toConfigDTOs(list, settings.CurrentConfig),
		Mode:           string(settings.Mode),
		Processes:      settings.Processes,
		ProcessFolders: settings.ProcessFolders,
		Targets:        settings.Targets,
		ExitTarget:     app.ExitTargetURL,
		Transport:      engine.TransportTCP,
		Elevated:       elevated,
		Conflicts:      toConflictDTOs(conflicts),
		SettingsError:  a.settingsErr,
		DataDir:        a.layout.Root,
		Version:        versionString(),
	}
	if settings.Transport == engine.TransportUDP {
		dto.Transport = engine.TransportUDP
	}
	if a.lockErr != nil {
		dto.LockError = "另一个程序正在使用分流核心，请关闭该程序后重新打开 GAMEBOOST"
	}
	return dto
}

// Start begins acceleration and returns non-fatal warnings.
func (a *App) Start() ([]string, error) {
	if a.lockErr != nil {
		return nil, errors.New("另一个程序正在使用分流核心，请先关闭该程序")
	}
	return a.ctl.Start()
}

// Stop ends acceleration.
func (a *App) Stop() error { return a.ctl.Stop() }

// RunningProcesses lists running acceleration targets for the stop confirmation.
func (a *App) RunningProcesses() []string { return a.ctl.RunningProcesses() }

// SelectConfig makes a file in configs\ the current node configuration.
func (a *App) SelectConfig(name string) error { return a.ctl.SelectConfig(name) }

func (a *App) SetMode(mode string) error {
	return a.ctl.SetMode(store.RouteMode(mode))
}

func (a *App) SetTargets(targets []string) ([]string, error) {
	if err := a.ctl.SetTargets(targets); err != nil {
		return nil, err
	}
	return a.ctl.Settings().Targets, nil
}

// PickProcessFiles asks for Windows executables to add to the process list.
func (a *App) PickProcessFiles() ([]string, error) {
	return runtime.OpenMultipleFilesDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "选择要加速的程序",
		Filters: []runtime.FileFilter{{DisplayName: "可执行文件 (*.exe)", Pattern: "*.exe"}},
	})
}

// PickProcessFolder asks for a directory whose programs should be accelerated.
func (a *App) PickProcessFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "选择要加速的程序文件夹",
	})
}

// SetProcessFolders saves the directories used for process routing.
func (a *App) SetProcessFolders(paths []string) ([]string, error) {
	if err := a.ctl.SetProcessFolders(paths); err != nil {
		return nil, err
	}
	return a.ctl.Settings().ProcessFolders, nil
}

// SetProcesses saves the custom acceleration targets.
func (a *App) SetProcesses(names []string) ([]string, error) {
	if err := a.ctl.SetProcesses(names); err != nil {
		return nil, err
	}
	return a.ctl.Settings().Processes, nil
}

// PickConfigFile asks for a configuration file; "" means cancelled.
func (a *App) PickConfigFile() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:   "选择节点配置",
		Filters: []runtime.FileFilter{{DisplayName: "节点配置 (*.json)", Pattern: "*.json"}},
	})
}

// ImportConfig copies a validated configuration into configs\. When a
// different file with the same name exists, Exists is set and nothing is
// written unless overwrite is true.
func (a *App) ImportConfig(path string, overwrite bool) (ImportResultDTO, error) {
	entry, err := a.layout.ImportConfig(path, overwrite)
	return a.finishImport(entry, err)
}

// ImportLink saves a mierus:// share link as a node JSON file.
func (a *App) ImportLink(name, link string, overwrite bool) (ImportResultDTO, error) {
	entry, err := a.layout.ImportLink(name, link, overwrite)
	return a.finishImport(entry, err)
}

func (a *App) finishImport(entry store.ConfigEntry, err error) (ImportResultDTO, error) {
	if errors.Is(err, store.ErrConfigExists) {
		return ImportResultDTO{Name: entry.Name, Exists: true}, nil
	}
	if err != nil {
		return ImportResultDTO{}, err
	}
	a.log.Printf(logx.App, "已导入节点配置 %s", entry.Name)
	if a.ctl.Settings().CurrentConfig == "" && a.ctl.Status().State == app.StateIdle {
		a.ctl.SelectConfig(entry.Name)
	}
	return ImportResultDTO{Name: entry.Name}, nil
}

// DeleteConfig removes a configuration; the one in use while accelerating
// cannot be deleted.
func (a *App) DeleteConfig(name string) error {
	st := a.ctl.Status()
	if st.State != app.StateIdle && strings.EqualFold(st.Config, name) {
		return errors.New("正在使用这个配置加速，请先停止加速")
	}
	if err := a.layout.DeleteConfig(name); err != nil {
		return err
	}
	a.log.Printf(logx.App, "已删除节点配置 %s", name)
	return nil
}

// OpenFolder opens configs, logs or the data folder in Explorer.
func (a *App) OpenFolder(kind string) error {
	dirs := map[string]string{"configs": a.layout.Configs, "logs": a.layout.Logs, "data": a.layout.Root}
	dir, ok := dirs[kind]
	if !ok {
		return fmt.Errorf("未知的文件夹：%q", kind)
	}
	return exec.Command("explorer.exe", dir).Start()
}

// SetTransport chooses TCP or UDP to the node for the next start.
func (a *App) SetTransport(transport string) error { return a.ctl.SetTransport(transport) }

// RunCheck runs the exit and DNS checks through the selected node.
func (a *App) RunCheck() (CheckDTO, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
	defer cancel()
	r, err := a.ctl.Check(ctx)
	if err != nil {
		return CheckDTO{}, err
	}
	return toCheckDTO(r), nil
}

// RecentLogs returns the latest log lines for the log page.
func (a *App) RecentLogs() []LogLineDTO {
	lines := a.log.Recent(500)
	out := make([]LogLineDTO, len(lines))
	for i, l := range lines {
		out[i] = toLogLineDTO(l)
	}
	return out
}

// ExportDiagnostics saves a support bundle; "" means cancelled.
func (a *App) ExportDiagnostics() (string, error) {
	path, err := runtime.SaveFileDialog(a.ctx, runtime.SaveDialogOptions{
		Title:           "导出诊断包",
		DefaultFilename: "GAMEBOOST-诊断-" + time.Now().Format("20060102-150405") + ".zip",
		Filters:         []runtime.FileFilter{{DisplayName: "压缩包 (*.zip)", Pattern: "*.zip"}},
	})
	if err != nil || path == "" {
		return "", err
	}
	elevated, conflicts := a.ctl.Preflight()
	summary := []string{
		"版本：" + versionString(),
		"数据目录：" + a.layout.Root,
		fmt.Sprintf("管理员权限：%v", elevated),
		"加速状态：" + string(a.ctl.Status().State),
		"分流组件：" + a.runtime.describe,
	}
	for _, name := range []string{redirector.WinDivertService, redirector.ProxyBridgeService} {
		state, err := redirector.ServiceState(name)
		if err != nil {
			state = err.Error()
		}
		summary = append(summary, fmt.Sprintf("服务 %s：%s", name, state))
	}
	for _, c := range conflicts {
		summary = append(summary, "冲突："+c.Message)
	}
	summary = append(summary, app.EnvironmentLines()...)
	settings := a.ctl.Settings()
	if settings.Mode == store.ModeIP {
		summary = append(summary, "代理模式：IP 模式", "目标 IP："+strings.Join(settings.Targets, "、"))
	} else {
		summary = append(summary, "代理模式：进程模式", "加速进程："+strings.Join(settings.Processes, "、"))
		summary = append(summary, "运行中的加速进程："+strings.Join(a.ctl.RunningProcesses(), "、"))
	}
	var buf bytes.Buffer
	if err := writeDiagnostics(&buf, a.layout, summary); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return "", err
	}
	a.log.Printf(logx.App, "已导出诊断包 %s", path)
	return path, nil
}

// FixConflict resolves a conflict the launcher can fix itself (see
// ConflictDTO.Fix) and reports what it did.
func (a *App) FixConflict(fix string) ([]string, error) { return a.ctl.FixConflict(fix) }

// Cleanup removes the driver service, firewall rules and runtime files.
func (a *App) Cleanup() ([]string, error) { return a.ctl.Cleanup(a.exePath) }

// About returns version and license information.
func (a *App) About() AboutDTO { return aboutInfo() }

// OpenURL opens an https link from the About page in the default browser.
func (a *App) OpenURL(url string) {
	if strings.HasPrefix(url, "https://") {
		runtime.BrowserOpenURL(a.ctx, url)
	}
}

// HideToTray hides the main window.
func (a *App) HideToTray() { runtime.WindowHide(a.ctx) }

// ShowWindow brings the main window back.
func (a *App) ShowWindow() {
	runtime.WindowUnminimise(a.ctx)
	runtime.WindowShow(a.ctx)
}

// Quit exits the program; acceleration is stopped first during shutdown.
func (a *App) Quit() {
	a.quitting.Store(true)
	runtime.Quit(a.ctx)
}
