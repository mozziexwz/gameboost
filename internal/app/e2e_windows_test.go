//go:build windows

package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/apis/constant"
	"github.com/enfein/mieru/v3/apis/model"
	mieruserver "github.com/enfein/mieru/v3/apis/server"
	pb "github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	mierucommon "github.com/enfein/mieru/v3/pkg/common"
	"google.golang.org/protobuf/proto"

	"github.com/mozziexwz/gameboost/internal/logx"
	"github.com/mozziexwz/gameboost/internal/probe"
	"github.com/mozziexwz/gameboost/internal/redirector"
	"github.com/mozziexwz/gameboost/internal/store"
	"github.com/mozziexwz/gameboost/internal/winsys"
)

const (
	e2eHit  = "127.0.0.219:54681"
	e2eMiss = "127.0.1.219:54681"
	e2eIPv6 = "[::1]:54681"

	// A destination ending in .255 outside the local subnet must still be proxied.
	e2eOutboundIP = "192.0.2.255"
	e2eOutbound   = e2eOutboundIP + ":54681"
)

// TestWindowsRoutingE2E uses real child processes, the signed WinDivert
// driver, ProxyBridge and an embedded loopback mieru peer. The peer echoes
// test traffic locally. Enable loopback matching only in this fixture so
// successes and bypasses can be tested without a remote host.
func TestWindowsRoutingE2E(t *testing.T) {
	if os.Getenv("GAMEBOOST_E2E") != "1" {
		t.Skip("需要管理员运行 scripts/e2e-windows.ps1")
	}
	if !winsys.IsElevated() {
		t.Fatal("需要管理员权限加载 WinDivert")
	}
	if state, _ := redirector.ServiceState(redirector.WinDivertService); state == "RUNNING" {
		t.Fatal("请先停止正在使用 WinDivert 的程序，再运行独立验收")
	}
	core := os.Getenv("MSBOOST_CORE_DLL")
	if core == "" {
		t.Fatal("缺少验收用分流 DLL 路径")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, name := range redirector.FirewallRuleNames(exe) {
			_, _ = winsys.DeleteFirewallRule(name)
		}
		_, _ = redirector.StopService(redirector.WinDivertService, 3*time.Second)
	}()
	nodePort := testMieruPeer(t, pb.TransportProtocol_TCP, 0, serveE2EPeer)
	layout := store.NewLayout(t.TempDir())
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	link := "mierus://local:test-fixture@127.0.0.1?profile=验收&protocol=TCP&port=" + strconv.Itoa(nodePort)
	node, err := layout.ImportLink("loopback", link, false)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := logx.New("", 0)
	ctl := NewWindows(layout, log, core, nil)
	ctl.deps.Environment = nil
	ctl.deps.SystemProxies = nil
	load := ctl.deps.LoadRedirector
	ctl.deps.LoadRedirector = func() (Redirector, error) {
		r, err := load()
		if err != nil {
			return nil, err
		}
		return loopbackTestRedirector{r.(coreAdapter)}, nil
	}
	defer func() {
		if t.Failed() {
			for _, l := range log.Recent(32) {
				t.Log(l.Text)
			}
		}
	}()
	defer ctl.Shutdown()
	if err := ctl.SelectConfig(node.Name); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binary, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(dir, "gameboost-selected.exe")
	other := filepath.Join(dir, "gameboost-other.exe")
	for _, path := range []string{selected, other} {
		if err := os.WriteFile(path, binary, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := ctl.SetProcesses([]string{selected}); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetTargets([]string{"127.0.0.0/24", e2eOutboundIP, "::1"}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []store.RouteMode{store.ModeProcess, store.ModeIP} {
		if err := ctl.SetMode(mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ctl.Start(); err != nil {
			t.Fatal(err)
		}
		for _, transport := range []string{"tcp4", "udp4"} {
			if mode == store.ModeProcess {
				runChild(t, selected, transport, e2eHit, true)
				runChild(t, other, transport, e2eHit, false)
			} else {
				runChild(t, other, transport, e2eHit, true)
				runChild(t, selected, transport, e2eMiss, false)
			}
			t.Logf("%s：%s 命中经节点回显，非命中旁路", mode, transport)
		}
		if os.Getenv("GAMEBOOST_E2E_OUTBOUND") == "1" {
			client := selected
			if mode == store.ModeIP {
				client = other
			}
			for _, protocol := range []string{"tcp4", "udp4"} {
				t.Run(string(mode)+"-"+protocol+"-outbound", func(t *testing.T) {
					runChild(t, client, protocol, e2eOutbound, true)
				})
			}
		}
		t.Run(string(mode)+"-ipv6", func(t *testing.T) {
			client := selected
			if mode == store.ModeIP {
				client = other
			}
			runChild(t, client, "tcp6", e2eIPv6, true)
		})
		stats := ctl.Status()
		if stats.Upload == 0 || stats.Download == 0 {
			t.Fatalf("%s 节点流量未累加：%+v", mode, stats)
		}
		t.Logf("%s：节点累计流量 上行 %d 字节，下行 %d 字节", mode, stats.Upload, stats.Download)
		if err := ctl.Stop(); err != nil {
			t.Fatal(err)
		}
		runChild(t, selected, "tcp4", e2eHit, false)
	}
	t.Run("process-folder", func(t *testing.T) {
		folder := filepath.Join(dir, "Game Files")
		nested := filepath.Join(folder, "bin", "x64")
		neighbor := filepath.Join(dir, "Game Files Extra")
		for _, path := range []string{nested, neighbor} {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := ctl.SetMode(store.ModeProcess); err != nil {
			t.Fatal(err)
		}
		if err := ctl.SetProcesses(nil); err != nil {
			t.Fatal(err)
		}
		if err := ctl.SetProcessFolders([]string{folder}); err != nil {
			t.Fatal(err)
		}
		if _, err := ctl.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := ctl.Stop(); err != nil {
				t.Error(err)
			}
		}()

		// Create the executables after start to exercise the live path rule.
		direct := filepath.Join(folder, "launcher.exe")
		selected := filepath.Join(nested, "game.exe")
		other := filepath.Join(neighbor, "game.exe")
		for _, path := range []string{direct, selected, other} {
			if err := os.WriteFile(path, binary, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		runChild(t, direct, "tcp4", e2eHit, true)
		for _, protocol := range []string{"tcp4", "udp4"} {
			runChild(t, selected, protocol, e2eHit, true)
			runChild(t, other, protocol, e2eHit, false)
		}
	})
}

type loopbackTestRedirector struct{ coreAdapter }

func (r loopbackTestRedirector) Prepare() {
	r.coreAdapter.Prepare()
	r.core.SetLocalhostViaProxy(true)
}

type peerListener struct{ net.Listener }

func (l peerListener) Listen(context.Context, string, string) (net.Listener, error) {
	return l.Listener, nil
}

type packetPeerListener struct{ net.PacketConn }

func (l packetPeerListener) ListenPacket(context.Context, string, string) (net.PacketConn, error) {
	return l.PacketConn, nil
}

type delayedPeerPackets struct {
	net.PacketConn
	delay time.Duration
}

func (c delayedPeerPackets) WriteTo(data []byte, addr net.Addr) (int, error) {
	// Delay packets independently so a delayed ACK cannot stall the peer's
	// output loop. Adding all of the RTT on one leg is enough for this fixture.
	packet := bytes.Clone(data)
	time.AfterFunc(c.delay, func() { c.PacketConn.WriteTo(packet, addr) })
	return len(data), nil
}

func testMieruPeer(t *testing.T, transport pb.TransportProtocol, packetDelay time.Duration, serve func(net.Conn, *model.Request)) int {
	t.Helper()
	cfg := &mieruserver.ServerConfig{
		Config: &pb.ServerConfig{
			ListenIPAddress: proto.String("127.0.0.1"),
			Users:           []*pb.User{{Name: proto.String("local"), Password: proto.String("test-fixture")}},
		},
	}
	var port int
	if transport == pb.TransportProtocol_UDP {
		ln, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		port = ln.LocalAddr().(*net.UDPAddr).Port
		if packetDelay > 0 {
			ln = delayedPeerPackets{PacketConn: ln, delay: packetDelay}
		}
		cfg.PacketListenerFactory = packetPeerListener{ln}
	} else {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		port = ln.Addr().(*net.TCPAddr).Port
		cfg.StreamListenerFactory = peerListener{ln}
	}
	cfg.Config.PortBindings = []*pb.PortBinding{{Port: proto.Int32(int32(port)), Protocol: transport.Enum()}}
	peer := mieruserver.NewServer()
	if err := peer.Store(cfg); err != nil {
		t.Fatal(err)
	}
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Stop() })
	go func() {
		for {
			conn, req, err := peer.Accept()
			if err != nil {
				return
			}
			go serve(conn, req)
		}
	}()
	return port
}

func TestMieruUDPWebExit(t *testing.T) {
	const relayRTT = 53 * time.Millisecond
	const responseDelay = 20 * time.Millisecond
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "ip.sb:443" || r.URL.Path != "/" {
			t.Errorf("unexpected web probe %s%s", r.Host, r.URL.Path)
		}
		time.Sleep(responseDelay)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer web.Close()
	port := testMieruPeer(t, pb.TransportProtocol_UDP, relayRTT, func(conn net.Conn, req *model.Request) {
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if req.Command != constant.Socks5ConnectCmd || req.DstAddr.String() != "ip.sb:443" {
			t.Errorf("unexpected node probe target %s", req.DstAddr.String())
			return
		}
		// A generic node may acknowledge the request before dialing.
		if model.WriteSocks5Response(conn, constant.Socks5ReplySuccess, model.AddrSpec{IP: net.IPv4zero}) != nil {
			return
		}
		dest, err := net.DialTimeout("tcp4", web.Listener.Addr().String(), time.Second)
		if err == nil {
			mierucommon.BidiCopy(conn, dest)
		}
	})
	layout := store.NewLayout(t.TempDir())
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	pattern, err := proto.Marshal(&pb.TrafficPattern{LowEntropy: &pb.LowEntropyPattern{Mode: pb.LowEntropyMode_LOW_ENTROPY_MODE_48.Enum()}})
	if err != nil {
		t.Fatal(err)
	}
	link := "mierus://local:test-fixture@127.0.0.1?profile=udp&protocol=UDP&port=" + strconv.Itoa(port) +
		"&handshake-mode=HANDSHAKE_NO_WAIT&traffic-pattern=" + url.QueryEscape(base64.StdEncoding.EncodeToString(pattern))
	node, err := layout.ImportLink("udp", link, false)
	if err != nil {
		t.Fatal(err)
	}
	log, _ := logx.New("", 0)
	ctl := NewWindows(layout, log, "", nil)
	ctl.deps.System = &fakeSystem{elevated: true}
	ctl.deps.Environment = nil
	ctl.deps.SystemProxies = nil
	ctl.deps.StartDNS = nil
	ctl.deps.LoadRedirector = func() (Redirector, error) { return newFakeRedirector(), nil }
	ctl.deps.Checks.ExitIP = func(context.Context, probe.Proxy) (string, error) {
		return "", errors.New("public exit-IP service unavailable in the local fixture")
	}
	ctl.deps.Checks.NodeDNS = func(context.Context, probe.Proxy) error { return nil }
	ctl.deps.Checks.Entrance = func(string, int, int, time.Duration) probe.Result { return probe.Result{} }
	ctl.deps.Checks.WebLatency = func(ctx context.Context, p probe.Proxy, target string) probe.Result {
		if target != "https://ip.sb/" {
			t.Fatalf("exit check used %s", target)
		}
		// The loopback fixture serves HTTP on the same logical destination.
		// Production uses HTTPS with certificate verification.
		return probe.HTTPResponseLatency(ctx, p, "http://ip.sb:443/")
	}
	defer ctl.Shutdown()
	if err := ctl.SelectConfig(node.Name); err != nil {
		t.Fatal(err)
	}
	if err := ctl.SetProcesses([]string{"game.exe"}); err != nil {
		t.Fatal(err)
	}
	if warnings, err := ctl.Start(); err != nil || len(warnings) != 0 || ctl.Status().Transport != "udp" {
		t.Fatalf("start over generic UDP peer: warnings=%v err=%v transport=%s", warnings, err, ctl.Status().Transport)
	}
	r, err := ctl.Check(context.Background())
	if err != nil || r.Exit.SuccessCount != 2 || r.ExitTarget != "https://ip.sb/" {
		t.Fatalf("expected two HTTP responses through the UDP relay: %+v, %v", r, err)
	}
	for _, sample := range r.Exit.Samples {
		if sample.Duration < relayRTT+responseDelay {
			t.Fatalf("sample returned before the HTTP response: %v", sample.Duration)
		}
	}
	t.Logf("HTTP response through the UDP relay: %s", r.Exit.Detail())
}

func serveE2EPeer(conn net.Conn, req *model.Request) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	bind := model.AddrSpec{IP: net.IPv4(127, 0, 0, 1), Port: 54681}
	switch req.Command {
	case constant.Socks5ConnectCmd:
		if addr := req.DstAddr.String(); addr != e2eHit && addr != e2eOutbound && addr != e2eIPv6 {
			model.WriteSocks5Response(conn, constant.Socks5ReplyNotAllowedByRuleSet, bind)
			return
		}
		if model.WriteSocks5Response(conn, constant.Socks5ReplySuccess, bind) == nil {
			io.Copy(conn, conn)
		}
	case constant.Socks5UDPAssociateCmd:
		if model.WriteSocks5Response(conn, constant.Socks5ReplySuccess, bind) != nil {
			return
		}
		tunnel := apicommon.NewPacketOverStreamTunnel(conn)
		var packet [65535]byte
		for {
			n, err := tunnel.Read(packet[:])
			if err != nil {
				return
			}
			if _, err := tunnel.Write(packet[:n]); err != nil {
				return
			}
		}
	}
}

func runChild(t *testing.T, exe, network, dest string, wantEcho bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestE2EClient$")
	cmd.Env = append(os.Environ(), "GAMEBOOST_E2E_CLIENT="+network, "GAMEBOOST_E2E_DEST="+dest, "GAMEBOOST_E2E_ECHO="+strconv.FormatBool(wantEcho))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s %s echo=%v: %v\n%s", filepath.Base(exe), network, dest, wantEcho, err, out)
	}
}

func TestE2EClient(t *testing.T) {
	network := os.Getenv("GAMEBOOST_E2E_CLIENT")
	if network == "" {
		t.Skip("验收子进程")
	}
	want := os.Getenv("GAMEBOOST_E2E_ECHO") == "true"
	dest := os.Getenv("GAMEBOOST_E2E_DEST")
	conn, err := net.DialTimeout(network, dest, 2500*time.Millisecond)
	if err != nil {
		if want {
			t.Fatal(err)
		}
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	data := bytes.Repeat([]byte("gameboost-e2e "), 53)
	_, err = conn.Write(data)
	result := make([]byte, len(data))
	if err == nil {
		_, err = io.ReadFull(conn, result)
	}
	echo := err == nil && bytes.Equal(result, data)
	if echo != want {
		t.Fatalf("echo=%v, want %v, error %v", echo, want, err)
	}
}
