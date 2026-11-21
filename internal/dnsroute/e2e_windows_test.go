//go:build windows

package dnsroute

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/windows"

	"github.com/mozziexwz/gameboost/internal/doh"
	"github.com/mozziexwz/gameboost/internal/redirector"
	"github.com/mozziexwz/gameboost/internal/winsys"
)

func divertDLL(t *testing.T) string {
	t.Helper()
	core := os.Getenv("MSBOOST_CORE_DLL")
	if core == "" {
		core = filepath.Join("..", "..", "build", "core", "ProxyBridgeCore.dll")
	}
	dll := filepath.Join(filepath.Dir(core), "WinDivert.dll")
	if _, err := os.Stat(dll); err != nil {
		t.Skipf("WinDivert 未构建：%v", err)
	}
	return dll
}

// Compile all three filters with the exact library used by the executable.
// This check doesn't need to load the kernel driver.
func TestDNSFiltersCompile(t *testing.T) {
	d, err := loadDriver(divertDLL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.library.Release()
	compile, err := d.library.FindProc("WinDivertHelperCompileFilter")
	if err != nil {
		t.Fatal(err)
	}
	primary, dropped, exposure := filters([]string{"10.0.0.53", "fe80::203"}, []doh.Flow{
		{Endpoint: netip.MustParseAddrPort("192.0.2.16:53"), UDP: true},
	}, map[uint16]uint16{51000: 53, 51001: 443, 51002: 853})
	for _, filter := range []string{primary, dropped, exposure} {
		text, _ := windows.BytePtrFromString(filter)
		var message *byte
		var pos uint32
		ok, _, _ := compile.Call(uintptr(unsafe.Pointer(text)), 0, 0, 0,
			uintptr(unsafe.Pointer(&message)), uintptr(unsafe.Pointer(&pos)))
		if ok == 0 {
			t.Fatalf("WinDivert filter at %d: %s (%s)", pos, windows.BytePtrToString(message), filter)
		}
	}
}

func TestWindowsDNSE2E(t *testing.T) {
	if os.Getenv("GAMEBOOST_E2E") != "1" {
		t.Skip("需要管理员运行 scripts/e2e-windows.ps1")
	}
	if !winsys.IsElevated() {
		t.Fatal("需要管理员权限加载 WinDivert")
	}
	if state, _ := redirector.ServiceState(redirector.WinDivertService); state == "RUNNING" {
		t.Fatal("请先停止正在使用 WinDivert 的程序，再运行独立验收")
	}
	defer redirector.StopService(redirector.WinDivertService, 3*time.Second)

	// Four real sockets on loopback exercise the Windows network stack
	// without sending a DNS query off the test machine.
	var direct, protected atomic.Int32
	var offline atomic.Bool
	var encrypted atomic.Int32
	for _, addr := range []string{"127.0.0.1:53", "[::1]:53"} {
		serveDecoy(t, addr, &direct)
	}
	s, err := start(divertDLL(t), []string{"192.0.2.54"}, nil, func(_ context.Context, request []byte) ([]byte, error) {
		protected.Add(1)
		if offline.Load() {
			return nil, errors.New("测试节点断线")
		}
		return fakeReply(request), nil
	}, func(_ context.Context, host string, port int) (net.Conn, error) {
		if host != "192.0.2.54" || (port != httpsPort && port != tlsPort) {
			return nil, errors.New("验收环境禁止外连")
		}
		encrypted.Add(1)
		local, peer := net.Pipe()
		go func() {
			defer peer.Close()
			io.Copy(peer, peer)
		}()
		return local, nil
	}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, test := range []struct{ network, address string }{
		{"udp4", "127.0.0.1:53"}, {"tcp4", "127.0.0.1:53"},
		{"udp6", "[::1]:53"}, {"tcp6", "[::1]:53"},
		{"udp4", "192.0.2.54:53"}, {"tcp4", "192.0.2.54:53"},
	} {
		checkDNS(t, test.network, test.address, dnsmessage.RCodeSuccess)
		t.Logf("%s %s 经 WinDivert 中继应答", test.network, test.address)
	}
	if protected.Load() < 4 || direct.Load() != 0 {
		t.Fatalf("保护请求 %d，旁路请求 %d", protected.Load(), direct.Load())
	}
	for _, port := range []int{httpsPort, tlsPort} {
		conn, err := net.DialTimeout("tcp4", fmt.Sprintf("192.0.2.54:%d", port), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		packet := []byte("encrypted-resolver-test")
		if _, err := conn.Write(packet); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		reply := make([]byte, len(packet))
		_, err = io.ReadFull(conn, reply)
		conn.Close()
		if err != nil || string(reply) != string(packet) {
			t.Fatalf("DNS TCP %d 未进入中继：%v", port, err)
		}
		t.Logf("已配置解析器的 TCP %d 经 WinDivert 进入代理中继", port)
	}
	if encrypted.Load() != 2 {
		t.Fatalf("加密 DNS 中继命中 %d 次，预期 2 次", encrypted.Load())
	}
	offline.Store(true)
	checkDNS(t, "udp4", "127.0.0.1:53", dnsmessage.RCodeServerFailure)
	checkDNS(t, "tcp6", "[::1]:53", dnsmessage.RCodeServerFailure)
	if direct.Load() != 0 {
		t.Fatalf("断线后有 %d 个请求发给原 DNS", direct.Load())
	}
	t.Log("节点断线返回 SERVFAIL，原 DNS 未收到请求")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	checkDNS(t, "udp4", "127.0.0.1:53", dnsmessage.RCodeSuccess)
	checkDNS(t, "tcp6", "[::1]:53", dnsmessage.RCodeSuccess)
	if direct.Load() != 2 {
		t.Fatalf("退出后原路径没有恢复，旁路请求 %d", direct.Load())
	}
	t.Log("关闭防护后原 DNS 路径恢复")
}

func checkDNS(t *testing.T, network, dest string, code dnsmessage.RCode) {
	t.Helper()
	conn, err := net.DialTimeout(network, dest, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	q := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0xcafe, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("gameboost-e2e.test."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	wire, _ := q.Pack()
	tcp := network[:3] == "tcp"
	if tcp {
		wire = binary.BigEndian.AppendUint16(nil, uint16(len(wire)))
		body, _ := q.Pack()
		wire = append(wire, body...)
	}
	if _, err := conn.Write(wire); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 2048)
	var n int
	if tcp {
		if _, err = io.ReadFull(conn, reply[:2]); err == nil {
			n = int(binary.BigEndian.Uint16(reply[:2]))
			if n > len(reply) {
				t.Fatal("超出测试 DNS 应答长度")
			}
			_, err = io.ReadFull(conn, reply[:n])
		}
	} else {
		n, err = conn.Read(reply)
	}
	if err != nil {
		t.Fatalf("%s %s DNS 读取失败：%v", network, dest, err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(reply[:n]); err != nil || m.Header.ID != q.Header.ID || m.Header.RCode != code {
		t.Fatalf("%s DNS 应答 %+v，错误 %v，预期 RCode %v", network, m.Header, err, code)
	}
}

func fakeReply(request []byte) []byte {
	var q dnsmessage.Message
	if q.Unpack(request) != nil || len(q.Questions) != 1 || q.Questions[0].Name.String() != "gameboost-e2e.test." {
		return nil
	}
	q.Header.Response = true
	q.Header.RecursionAvailable = true
	q.Answers = []dnsmessage.Resource{{
		Header: dnsmessage.ResourceHeader{Name: q.Questions[0].Name, Class: dnsmessage.ClassINET, TTL: 1},
		Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 19}},
	}}
	body, _ := q.Pack()
	return body
}

func serveDecoy(t *testing.T, address string, hits *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("测试端口 %s 被占用：%v", address, err)
	}
	t.Cleanup(func() { ln.Close() })
	udp, err := net.ListenPacket("udp", address)
	if err != nil {
		t.Fatalf("测试端口 %s 被占用：%v", address, err)
	}
	t.Cleanup(func() { udp.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				serveDNSStream(context.Background(), c, func(_ context.Context, q []byte) ([]byte, error) {
					hits.Add(1)
					return fakeReply(q), nil
				})
			}()
		}
	}()
	go func() {
		var buf [2048]byte
		for {
			n, addr, err := udp.ReadFrom(buf[:])
			if err != nil {
				return
			}
			hits.Add(1)
			if reply := fakeReply(buf[:n]); reply != nil {
				udp.WriteTo(reply, addr)
			}
		}
	}()
}
