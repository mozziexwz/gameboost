// Package app is the launcher's orchestration layer: it owns the start/stop
// state machine (with rollback), live addon switching, the network check and
// cleanup. The UI and the developer CLI both drive acceleration through it.
//
// The embedded core, the redirector DLL and the operating system are reached
// through small interfaces so every step and every failure can be tested.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	mlog "github.com/enfein/mieru/v3/pkg/log"

	"github.com/mozziexwz/gameboost/internal/doh"
	"github.com/mozziexwz/gameboost/internal/engine"
	"github.com/mozziexwz/gameboost/internal/logx"
	"github.com/mozziexwz/gameboost/internal/probe"
	"github.com/mozziexwz/gameboost/internal/store"
	"github.com/mozziexwz/gameboost/internal/sysproxy"
)

// CoreEngine is a running embedded core (internal/engine.Engine).
type CoreEngine interface {
	Addr() *net.TCPAddr
	Credential() (user, password string)
	Stats() engine.Stats
	DNSFlows() []doh.Flow
	Close() error
}

// Redirector is the process redirector (ProxyBridgeCore.dll).
type Redirector interface {
	// Prepare turns on log delivery, turns off per-connection traffic logging
	// and keeps localhost traffic direct.
	Prepare()
	AddSOCKS5(host string, port uint16, user, password string, sendDomain bool) (uint32, error)
	DeleteProxy(id uint32) bool
	// AddProxyRule proxies TCP and UDP matching the process and host lists.
	AddProxyRule(processes, hosts string, proxyID uint32) (uint32, error)
	// AddTCPRule proxies the processes' TCP connections to the listed hosts
	// and ports (";"-separated) to proxyID; EditTCPRule changes such a rule
	// in place, keeping its position in the evaluation order.
	AddTCPRule(processes, hosts, ports string, proxyID uint32) (uint32, error)
	EditTCPRule(id uint32, processes, hosts, ports string, proxyID uint32) error
	EnableRule(id uint32) bool
	DisableRule(id uint32) bool
	DeleteRule(id uint32) bool
	// SetLocalhostProxyPorts lists loopback TCP ports that rules may proxy
	// while other localhost traffic stays direct (patch P4).
	SetLocalhostProxyPorts(ports []uint16) error
	// TraceConnections passes the redirector's connection decisions to fn
	// until stop is called. Call it before Start and call stop only after
	// Stop: the redirector reads the callback without a lock.
	TraceConnections(fn func(ConnEvent)) (stop func())
	Start() bool
	Stop() bool
}

// Conflict is something that would interfere with acceleration. Fix, when
// set, names a fix FixConflict can apply.
type Conflict struct {
	Message  string
	Blocking bool
	Fix      string
}

// System is the operating system as seen by the controller.
type System interface {
	Elevated() bool
	Conflicts() []Conflict
	RunningProcesses(names []string) []string
	// StopDriver stops the packet driver service and reports its state.
	StopDriver() (string, error)
	// DriverUnloading reports whether the packet driver is still being
	// removed (for example while another WinDivert program let go of it);
	// it cannot be started again until that finishes.
	DriverUnloading() bool
	// Cleanup removes everything the launcher may leave on the system and
	// reports what it did.
	Cleanup(exePath string) []string
	// FixConflict applies a conflict's fix and reports what it did.
	FixConflict(fix string) ([]string, error)
}

// CheckFuncs are the network checks; nil fields use the probe package.
type CheckFuncs struct {
	ExitIP          func(ctx context.Context, p probe.Proxy) (string, error)
	ResolveEntrance func(address string, timeout time.Duration) (ip, reason string)
	Entrance        func(ip string, port, count int, timeout time.Duration) probe.Result
	WebLatency      func(ctx context.Context, p probe.Proxy, url string) probe.Result
	NodeDNS         func(ctx context.Context, p probe.Proxy) error
	ConfirmRelay    func(ctx context.Context, p probe.Proxy, addr string, port int) error
}

func (c *CheckFuncs) fillDefaults() {
	if c.ExitIP == nil {
		c.ExitIP = probe.ExitIPv4
	}
	if c.ResolveEntrance == nil {
		c.ResolveEntrance = probe.ResolveRelay
	}
	if c.Entrance == nil {
		c.Entrance = probe.EntranceLatency
	}
	if c.WebLatency == nil {
		c.WebLatency = probe.HTTPResponseLatency
	}
	if c.NodeDNS == nil {
		c.NodeDNS = func(ctx context.Context, p probe.Proxy) error {
			resolver := doh.New(proxyDialer(p))
			defer resolver.Close()
			_, err := resolver.LookupIP(ctx, "ip4", "example.com")
			return err
		}
	}
	if c.ConfirmRelay == nil {
		c.ConfirmRelay = probe.ConfirmRelay
	}
}

// Deps wires the controller to real or fake implementations.
type Deps struct {
	Layout         store.Layout
	Log            *logx.Logger
	StartEngine    func(*engine.Config) (CoreEngine, error)
	LoadRedirector func() (Redirector, error) // called once, on first start
	// StartDNS protects machine DNS while the acceleration session is active.
	StartDNS func(probe.Proxy, []doh.Flow) (io.Closer, error)
	// PrepareRuntime, when set, checks the redirector's files before every
	// start and keeps them locked until release is called after the start.
	PrepareRuntime func() (release func(), err error)
	// SystemProxies, when set, returns the Windows system proxy endpoints;
	// the game's connections to them are then taken over (sysproxy.go).
	SystemProxies func() []sysproxy.Endpoint
	// Environment, when set, describes the network for the log at every
	// start: adapters, addresses, DNS servers and the system proxy.
	Environment func() []string
	System      System
	Checks      CheckFuncs
}

// State is the acceleration state.
type State string

const (
	StateIdle     State = "idle"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
)

// Status is a snapshot for the UI. Traffic is with the node, in bytes.
type Status struct {
	State        State
	Config       string
	Since        time.Time
	Accepted     int64
	Active       int64
	Upload       int64
	Download     int64
	UploadRate   int64 // bytes per second
	DownloadRate int64
	SystemProxy  []string // system proxy endpoints being taken over
	Transport    string   // active transport to the node
	LastError    string
}

// Errors the UI may want to distinguish.
var (
	ErrBusy        = errors.New("正在启动或停止，请稍后再试")
	ErrRunning     = errors.New("加速进行中，请先停止加速")
	ErrNotRunning  = errors.New("当前没有在加速")
	ErrNoConfig    = errors.New("请先选择节点配置")
	ErrNoProcesses = errors.New("请先添加并保存至少一个加速进程或文件夹")
	ErrNoTargets   = errors.New("请先添加并保存至少一个目标 IP 或网段")
)

type connection struct {
	proxy     probe.Proxy
	entrance  probe.RelayTarget
	transport string
	eng       CoreEngine
}

type session struct {
	config string
	connection
	proxyID uint32
	rules   []uint32
	since   time.Time
	secrets []string

	takeover *proxyTakeover // nil when SystemProxies is not set
	dns      io.Closer

	diag      *connDiag
	stopTrace func() // ends TraceConnections; only after the redirector stopped
	observed  bool   // the engine reports its connections to diag
}

// Controller is safe for concurrent use.
type Controller struct {
	deps Deps
	log  *logx.Logger

	mu        sync.Mutex
	state     State
	settings  store.Settings
	session   *session
	red       Redirector
	lastErr   string
	watchers  map[int]func(Status)
	nextWatch int
}

// New loads the settings and prepares the controller. A damaged settings file
// is reported in the log and replaced by the defaults in memory.
func New(deps Deps) *Controller {
	deps.Checks.fillDefaults()
	log := deps.Log
	if log == nil {
		log, _ = logx.New("", 0)
		deps.Log = log
	}
	mlog.SetOutput(log.Writer(logx.Core))
	settings, err := deps.Layout.LoadSettings()
	if err != nil {
		log.Printf(logx.App, "%v", err)
	}
	return &Controller{deps: deps, log: log, state: StateIdle, settings: settings, watchers: make(map[int]func(Status))}
}

// Settings returns a copy of the current settings.
func (c *Controller) Settings() store.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return copySettings(c.settings)
}

// Status returns the current state.
func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *Controller) statusLocked() Status {
	st := Status{State: c.state, Config: c.settings.CurrentConfig, LastError: c.lastErr}
	if c.session != nil {
		st.Config = c.session.config
		st.Transport = c.session.transport
		st.Since = c.session.since
		stats := c.session.eng.Stats()
		st.Accepted, st.Active = stats.Accepted, stats.Active
		st.Upload, st.Download = stats.Upload, stats.Download
		st.UploadRate, st.DownloadRate = stats.UploadRate, stats.DownloadRate
		if t := c.session.takeover; t != nil {
			st.SystemProxy = t.endpointNames()
		}
	}
	return st
}

// Watch calls fn after every state change until cancel is called.
func (c *Controller) Watch(fn func(Status)) (cancel func()) {
	c.mu.Lock()
	id := c.nextWatch
	c.nextWatch++
	c.watchers[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.watchers, id)
		c.mu.Unlock()
	}
}

func (c *Controller) emit() {
	c.mu.Lock()
	st := c.statusLocked()
	fns := make([]func(Status), 0, len(c.watchers))
	for _, fn := range c.watchers {
		fns = append(fns, fn)
	}
	c.mu.Unlock()
	for _, fn := range fns {
		fn(st)
	}
}

// SelectConfig makes name (a file in configs\) the configuration to use.
func (c *Controller) SelectConfig(name string) error {
	c.mu.Lock()
	if c.state != StateIdle {
		c.mu.Unlock()
		return ErrRunning
	}
	if _, err := c.deps.Layout.LoadConfig(name); err != nil {
		c.mu.Unlock()
		return err
	}
	next := copySettings(c.settings)
	next.CurrentConfig = name
	err := c.deps.Layout.SaveSettings(next)
	if err == nil {
		c.settings = next
	}
	c.mu.Unlock()
	if err == nil {
		c.log.Printf(logx.App, "已选择节点配置 %s", name)
		c.emit()
	}
	return err
}

// SetMode selects which list is active, keeping both lists for the next start.
func (c *Controller) SetMode(mode store.RouteMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateIdle {
		return ErrRunning
	}
	next := copySettings(c.settings)
	next.Mode = mode
	if err := c.deps.Layout.SaveSettings(next); err != nil {
		return err
	}
	c.settings = next
	return nil
}

// SetTargets saves the destination IPs and CIDR networks for IP mode.
func (c *Controller) SetTargets(values []string) error {
	targets, err := store.NormalizeTargets(values)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateIdle {
		return ErrRunning
	}
	next := copySettings(c.settings)
	next.Targets = targets
	if err := c.deps.Layout.SaveSettings(next); err != nil {
		return err
	}
	c.settings = next
	return nil
}

// SetProcesses replaces the list that will be used at the next start.
func (c *Controller) SetProcesses(names []string) error {
	processes := make([]string, 0, len(names))
	seen := make(map[string]bool)
	for _, name := range names {
		name = strings.ReplaceAll(strings.TrimSpace(name), "/", "\\")
		key := strings.ToLower(name)
		if name != "" && !seen[key] {
			processes = append(processes, name)
			seen[key] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateIdle {
		return ErrRunning
	}
	next := copySettings(c.settings)
	next.Processes = processes
	if err := c.deps.Layout.SaveSettings(next); err != nil {
		return err
	}
	c.settings = next
	return nil
}

// SetProcessFolders saves the directories whose executables will be accelerated.
func (c *Controller) SetProcessFolders(values []string) error {
	folders, err := store.NormalizeProcessFolders(values)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateIdle {
		return ErrRunning
	}
	next := copySettings(c.settings)
	next.ProcessFolders = folders
	if err := c.deps.Layout.SaveSettings(next); err != nil {
		return err
	}
	c.settings = next
	return nil
}

// SetTransport chooses TCP or UDP between the core and the node, for the next
// start. A configuration without a UDP binding always uses TCP.
func (c *Controller) SetTransport(transport string) error {
	if transport != engine.TransportTCP && transport != engine.TransportUDP {
		return fmt.Errorf("未知的连接方式：%q", transport)
	}
	c.mu.Lock()
	if c.state != StateIdle {
		c.mu.Unlock()
		return ErrRunning
	}
	next := copySettings(c.settings)
	next.Transport = transport
	err := c.deps.Layout.SaveSettings(next)
	if err == nil {
		c.settings = next
	}
	c.mu.Unlock()
	if err == nil {
		c.log.Printf(logx.App, "连接方式改为 %s", strings.ToUpper(transport))
		c.emit()
	}
	return err
}

// startTransport is how a start reaches the node: UDP when the user chose it
// and the configuration has a UDP binding, otherwise TCP, unless the
// configuration has nothing but UDP.
func startTransport(cfg *engine.Config, chosen string) string {
	tcp, udp := cfg.Transports()
	if udp && (chosen == engine.TransportUDP || !tcp) {
		return engine.TransportUDP
	}
	return engine.TransportTCP
}

// startConnection starts the core on the bindings of one transport of cfg.
func (c *Controller) startConnection(cfg *engine.Config, transport string) (connection, error) {
	run, err := cfg.ForTransport(transport)
	if err != nil {
		return connection{}, err
	}
	eng, err := c.deps.StartEngine(run)
	if err != nil {
		return connection{}, err
	}
	user, password := eng.Credential()
	proxy := probe.Proxy{Addr: eng.Addr().String(), User: user, Password: password}
	if node, ok := eng.(interface {
		DialNode(context.Context) (net.Conn, error)
	}); ok {
		proxy.DialNode = node.DialNode
	}
	return connection{proxy: proxy, entrance: probe.RelayTargetFromProfile(cfg.Profile), transport: transport, eng: eng}, nil
}

// udpCheckTimeout bounds both ordinary UDP tunnel checks at startup.
const udpCheckTimeout = 5 * time.Second

const exitTargetHost = "ip.sb"

// ExitTargetURL is the fixed HTTPS endpoint for the exit latency check.
const ExitTargetURL = "https://" + exitTargetHost + "/"

func (c *Controller) verifyUDP(proxy probe.Proxy) error {
	ctx, cancel := context.WithTimeout(context.Background(), udpCheckTimeout)
	defer cancel()
	targetCtx, cancelTarget := context.WithTimeout(ctx, udpCheckTimeout/2)
	targetErr := c.deps.Checks.ConfirmRelay(targetCtx, proxy, exitTargetHost, 443)
	cancelTarget()
	if targetErr == nil {
		return nil
	}
	// A slow target can outlive the preflight and never send a SOCKS reply.
	// An ordinary HTTPS response independently verifies the same tunnel.
	if _, err := c.deps.Checks.ExitIP(ctx, proxy); err != nil {
		return fmt.Errorf("目标连接：%w；出口 IP：%w", targetErr, err)
	}
	return nil
}

func (c *Controller) openConnection(cfg *engine.Config, chosenTransport string, verifyUDP bool) (connection, string, error) {
	transport := startTransport(cfg, chosenTransport)
	link, err := c.startConnection(cfg, transport)
	if err != nil || !verifyUDP || transport != engine.TransportUDP {
		return link, "", err
	}
	if err := c.verifyUDP(link.proxy); err != nil {
		link.eng.Close()
		if tcp, _ := cfg.Transports(); !tcp {
			return connection{}, "", fmt.Errorf("UDP 连接检测未通过：%w", err)
		}
		c.log.Printf(logx.App, "UDP 连接检测未通过（%v），本次改用 TCP", err)
		fallback, tcpErr := c.startConnection(cfg, engine.TransportTCP)
		return fallback, "UDP 连接检测未通过，本次改用 TCP 加速", tcpErr
	}
	return link, "", nil
}

// Start begins acceleration. Warnings are non-fatal findings for the user.
// On failure every step already taken is undone in reverse order.
func (c *Controller) Start() (warnings []string, err error) {
	c.mu.Lock()
	switch c.state {
	case StateRunning:
		c.mu.Unlock()
		return nil, ErrRunning
	case StateStarting, StateStopping:
		c.mu.Unlock()
		return nil, ErrBusy
	}
	c.state = StateStarting
	c.lastErr = ""
	settings := copySettings(c.settings)
	c.mu.Unlock()
	c.emit()

	sess, warnings, err := c.startSession(settings)

	c.mu.Lock()
	if err != nil {
		c.state = StateIdle
		c.lastErr = err.Error()
	} else {
		c.state = StateRunning
		c.session = sess
		if sess.takeover != nil {
			c.watchSystemProxy(sess.takeover)
		}
	}
	c.mu.Unlock()
	c.emit()
	return warnings, err
}

func (c *Controller) startSession(s store.Settings) (sess *session, warnings []string, err error) {
	var undo []func()
	var secrets []string
	defer func() {
		if err == nil {
			return
		}
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
		for _, s := range secrets {
			c.log.RemoveSecret(s)
		}
		c.log.Printf(logx.App, "开始加速失败：%v", err)
	}()

	sys := c.deps.System
	if !sys.Elevated() {
		return nil, nil, errors.New("需要以管理员身份运行 GAMEBOOST")
	}
	for _, conflict := range sys.Conflicts() {
		if conflict.Blocking {
			return nil, nil, errors.New(conflict.Message)
		}
		warnings = append(warnings, conflict.Message)
	}
	if s.CurrentConfig == "" {
		return nil, nil, ErrNoConfig
	}
	programs, hosts := s.RouteRule()
	if programs == "" {
		return nil, nil, ErrNoProcesses
	}
	if hosts == "" {
		return nil, nil, ErrNoTargets
	}
	if err := c.waitForDriver(); err != nil {
		return nil, nil, err
	}
	if prepare := c.deps.PrepareRuntime; prepare != nil {
		release, err := prepare()
		if err != nil {
			return nil, nil, fmt.Errorf("分流组件检查失败：%w", err)
		}
		// Held until the DLL is loaded and the driver has started.
		defer release()
	}
	cfg, err := c.deps.Layout.LoadConfig(s.CurrentConfig)
	if err != nil {
		return nil, nil, err
	}
	if pw := cfg.Profile.GetUser().GetPassword(); pw != "" {
		c.log.AddSecret(pw)
		secrets = append(secrets, pw)
	}
	if env := c.deps.Environment; env != nil {
		for _, line := range env() {
			c.log.Printf(logx.App, "%s", line)
		}
	}

	link, warning, err := c.openConnection(cfg, s.Transport, true)
	if err != nil {
		return nil, nil, err
	}
	if warning != "" {
		warnings = append(warnings, warning)
	}
	undo = append(undo, func() { link.eng.Close() })
	port := uint16(link.eng.Addr().Port)
	c.log.Printf(logx.App, "mieru 客户端已启动（%s），本地端口 %d", strings.ToUpper(link.transport), port)
	user, password := link.proxy.User, link.proxy.Password
	for _, secret := range []string{user, password} {
		if secret == "" {
			continue
		}
		c.log.AddSecret(secret)
		secrets = append(secrets, secret)
	}

	diag := newConnDiag(c.log)
	diag.setPrograms(programs)
	diag.ipMode = s.Mode == store.ModeIP
	diag.setProxy(int(port), "加速")
	observed := false
	if o, ok := link.eng.(observer); ok {
		o.Observe(diag.request)
		observed = true
	}

	red, err := c.redirector()
	if err != nil {
		return nil, nil, err
	}
	red.Prepare()
	proxyID, err := red.AddSOCKS5("127.0.0.1", port, user, password, s.Mode == store.ModeProcess)
	if err != nil {
		return nil, nil, err
	}
	undo = append(undo, func() { red.DeleteProxy(proxyID) })

	var rules []uint32
	// The system-proxy rule must come first: rules match in order.
	var takeover *proxyTakeover
	if s.Mode == store.ModeProcess && c.deps.SystemProxies != nil {
		if takeover, err = c.startTakeover(red, link.proxy, programs, &undo, &secrets); err != nil {
			return nil, nil, err
		}
		rules = append(rules, takeover.ruleID)
		diag.setProxy(takeover.server.Addr().Port, "改由 GAMEBOOST 接管（系统代理）")
	}
	ruleID, err := red.AddProxyRule(programs, hosts, proxyID)
	if err != nil {
		return nil, nil, err
	}
	rules = append(rules, ruleID)
	undo = append(undo, func() { red.DeleteRule(ruleID) })
	var dns io.Closer
	if c.deps.StartDNS != nil {
		dns, err = c.deps.StartDNS(link.proxy, link.eng.DNSFlows())
		if err != nil {
			return nil, nil, fmt.Errorf("无法启用 DNS 防泄露：%w", err)
		}
		undo = append(undo, func() { dns.Close() })
	}
	stopTrace := red.TraceConnections(diag.decision)
	undo = append(undo, stopTrace)
	if !red.Start() {
		if sys.DriverUnloading() {
			return nil, nil, errDriverUnloading
		}
		return nil, nil, errors.New("分流启动失败，详细原因见日志")
	}

	if s.Mode == store.ModeProcess {
		if running := sys.RunningProcesses(s.ProcessRules()); len(running) > 0 {
			warnings = append(warnings, "这些进程在加速开始前已在运行："+strings.Join(running, "、")+"。请重启对应程序，让它重新建立连接")
		}
	}
	c.log.Printf(logx.App, "加速已开始（配置 %s）", s.CurrentConfig)
	for _, w := range warnings {
		c.log.Printf(logx.App, "提醒：%s", w)
	}
	return &session{
		config: s.CurrentConfig, connection: link, proxyID: proxyID,
		rules: rules, since: time.Now(), secrets: secrets, takeover: takeover, dns: dns,
		diag: diag, stopTrace: stopTrace, observed: observed,
	}, warnings, nil
}

// observer is implemented by engines that report the connections they serve
// (engine.Engine); the fakes in tests may leave it out.
type observer interface {
	Observe(fn func(engine.Request))
}

// waitIdle waits up to max for an observed engine's connections to close, so
// their records are written before the session summary.
func waitIdle(eng CoreEngine, max time.Duration) {
	deadline := time.Now().Add(max)
	for eng.Stats().Active > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

// How long a start waits for a previous packet driver to finish unloading.
var (
	driverWait = 60 * time.Second
	driverPoll = time.Second
)

var errDriverUnloading = errors.New("分流驱动一直没有卸载完成。请关闭其他使用分流驱动的软件（如 ProxyBridge、GoodbyeDPI）后再试；如果仍然不行，重启电脑即可")

// waitForDriver waits while the packet driver is still being removed, which
// happens for a while after another WinDivert program (such as ProxyBridge)
// has let go of it; starting before that fails with error 1058.
func (c *Controller) waitForDriver() error {
	sys := c.deps.System
	if !sys.DriverUnloading() {
		return nil
	}
	c.log.Printf(logx.App, "分流驱动还在卸载（通常是刚关闭的其他分流软件还占用着它），正在等待，最多 %d 秒", int(driverWait/time.Second))
	deadline := time.Now().Add(driverWait)
	for sys.DriverUnloading() {
		if time.Now().After(deadline) {
			return errDriverUnloading
		}
		time.Sleep(driverPoll)
	}
	c.log.Printf(logx.App, "分流驱动已卸载完成，继续开始加速")
	return nil
}

func (c *Controller) redirector() (Redirector, error) {
	c.mu.Lock()
	red := c.red
	c.mu.Unlock()
	if red != nil {
		return red, nil
	}
	red, err := c.deps.LoadRedirector()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.red = red
	c.mu.Unlock()
	return red, nil
}

// Stop ends acceleration: interception first, then rules, the driver and the
// core. Existing game connections through the node are closed.
func (c *Controller) Stop() error {
	c.mu.Lock()
	switch c.state {
	case StateIdle:
		c.mu.Unlock()
		return ErrNotRunning
	case StateStarting, StateStopping:
		c.mu.Unlock()
		return ErrBusy
	}
	c.state = StateStopping
	sess, red := c.session, c.red
	c.mu.Unlock()
	c.emit()
	if sess.takeover != nil {
		sess.takeover.stopWatching() // outside c.mu: the watcher takes it
	}

	started := time.Now()
	if !red.Stop() {
		c.log.Printf(logx.App, "分流已处于停止状态")
	}
	if sess.stopTrace != nil {
		sess.stopTrace() // only now: the redirector no longer calls back
	}
	for i := len(sess.rules) - 1; i >= 0; i-- {
		red.DeleteRule(sess.rules[i])
	}
	red.DeleteProxy(sess.proxyID)
	if sess.takeover != nil {
		c.stopTakeover(red, sess.takeover)
	}
	if sess.dns != nil {
		if err := sess.dns.Close(); err != nil {
			c.log.Printf(logx.App, "关闭 DNS 防泄露：%v", err)
		}
	}
	if state, err := c.deps.System.StopDriver(); err != nil {
		c.log.Printf(logx.App, "驱动未能卸载（%s）：%v。如果其他软件正在使用它，这是正常的", state, err)
	} else {
		c.log.Printf(logx.App, "驱动状态：%s", state)
	}
	stats := sess.eng.Stats()
	sess.eng.Close()
	if sess.observed {
		waitIdle(sess.eng, 2*time.Second)
	}
	c.log.Printf(logx.App, "本次已使用流量 %s", FormatBytes(stats.Upload+stats.Download))
	for _, s := range sess.secrets {
		c.log.RemoveSecret(s)
	}
	c.log.Printf(logx.App, "加速已停止，用时 %v", time.Since(started).Round(time.Millisecond))
	if sess.diag != nil {
		sess.diag.finish()
	}

	c.mu.Lock()
	c.state = StateIdle
	c.session = nil
	c.mu.Unlock()
	c.emit()
	return nil
}

// Shutdown stops acceleration if needed; call it before the program exits.
func (c *Controller) Shutdown() {
	if c.Status().State == StateRunning {
		c.Stop()
	}
}

// Preflight reports elevation and current conflicts without starting anything.
// Conflicts only matter before a start, and while a session is active our own
// relay holds the port the check looks for, so none are reported then.
func (c *Controller) Preflight() (elevated bool, conflicts []Conflict) {
	if c.Status().State != StateIdle {
		return c.deps.System.Elevated(), nil
	}
	return c.deps.System.Elevated(), c.deps.System.Conflicts()
}

// RunningProcesses lists configured processes that are running now, so the UI
// can warn before stopping or starting acceleration.
func (c *Controller) RunningProcesses() []string {
	s := c.Settings()
	if s.Mode == store.ModeIP {
		return nil
	}
	return c.deps.System.RunningProcesses(s.ProcessRules())
}

// Cleanup removes what the launcher may leave on the system. It refuses to
// run while accelerating.
func (c *Controller) Cleanup(exePath string) ([]string, error) {
	c.mu.Lock()
	idle := c.state == StateIdle
	c.mu.Unlock()
	if !idle {
		return nil, ErrRunning
	}
	report := c.deps.System.Cleanup(exePath)
	for _, line := range report {
		c.log.Printf(logx.App, "清理：%s", line)
	}
	return report, nil
}

// FixConflict resolves a conflict that the launcher can resolve itself, such
// as stopping the official ProxyBridge. Only while idle.
func (c *Controller) FixConflict(fix string) ([]string, error) {
	c.mu.Lock()
	idle := c.state == StateIdle
	c.mu.Unlock()
	if !idle {
		return nil, ErrRunning
	}
	report, err := c.deps.System.FixConflict(fix)
	for _, line := range report {
		c.log.Printf(logx.App, "处理冲突：%s", line)
	}
	if err != nil {
		c.log.Printf(logx.App, "处理冲突失败：%v", err)
	}
	return report, err
}

// CheckResult holds independent observations of the node and the exit path.
type CheckResult struct {
	ExitIP         string
	ExitIPError    string
	EntranceTarget string // "ip:port" when one entrance could be chosen
	Entrance       probe.Result
	ExitTarget     string
	Exit           probe.Result
	DNSError       string
	// Transport is the node transport or the selected IP proxy protocol.
	Transport string
}

// Check looks up the public exit IPv4, times the node's TCP entrance, and
// measures the HTTPS response from ip.sb over the node. When idle it opens a temporary
// engine, so the check does not require the redirector.
func (c *Controller) Check(ctx context.Context) (CheckResult, error) {
	c.mu.Lock()
	state, sess, s := c.state, c.session, copySettings(c.settings)
	c.mu.Unlock()

	var link connection
	switch state {
	case StateRunning:
		link = sess.connection
	case StateIdle:
		if s.CurrentConfig == "" {
			return CheckResult{}, ErrNoConfig
		}
		cfg, err := c.deps.Layout.LoadConfig(s.CurrentConfig)
		if err != nil {
			return CheckResult{}, err
		}
		// Without a fallback: a UDP check that fails shows the user why.
		link, _, err = c.openConnection(cfg, s.Transport, false)
		if err != nil {
			return CheckResult{}, err
		}
		defer link.eng.Close()
	default:
		return CheckResult{}, ErrBusy
	}

	checks := c.deps.Checks
	dnsResult := make(chan error, 1)
	go func() { dnsResult <- checks.NodeDNS(ctx, link.proxy) }()
	result := CheckResult{
		ExitTarget: ExitTargetURL,
		Transport:  link.transport,
	}

	if ip, err := checks.ExitIP(ctx, link.proxy); err != nil {
		result.ExitIPError = err.Error()
	} else {
		result.ExitIP = ip
	}

	target := link.entrance
	switch {
	case target.Err != "":
		result.Entrance = probe.Unavailable(target.Err)
	default:
		if ip, reason := checks.ResolveEntrance(target.Address, probe.AttemptTimeout); reason != "" {
			result.Entrance = probe.Unavailable(reason)
		} else {
			result.EntranceTarget = fmt.Sprintf("%s:%d", ip, target.Port)
			result.Entrance = checks.Entrance(ip, target.Port, probe.DefaultSampleCount, probe.AttemptTimeout)
		}
	}

	result.Exit = checks.WebLatency(ctx, link.proxy, result.ExitTarget)
	if err := <-dnsResult; err != nil {
		result.DNSError = err.Error()
	}
	c.log.Printf(logx.App, "出口检测（%s）：出口 IPv4 %s，入口 %s，目标 %s %s，节点 DNS %s", strings.ToUpper(link.transport),
		orText(result.ExitIP, result.ExitIPError), result.Entrance.Compact(), result.ExitTarget, result.Exit.Compact(), orText(result.DNSError, "正常"))
	return result, nil
}

func orText(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func copySettings(s store.Settings) store.Settings {
	s.Processes = append([]string{}, s.Processes...)
	s.ProcessFolders = append([]string{}, s.ProcessFolders...)
	s.Targets = append([]string{}, s.Targets...)
	return s
}
