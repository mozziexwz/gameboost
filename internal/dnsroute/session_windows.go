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
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"

	"github.com/mozziexwz/gameboost/internal/doh"
)

type dialFunc = func(context.Context, string, int) (net.Conn, error)

var dnsFlush = windows.NewLazySystemDLL("dnsapi.dll").NewProc("DnsFlushResolverCache")

type Session struct {
	driver    *driver
	packet    uintptr
	tailDrop  uintptr
	portDrop  uintptr
	listeners map[uint16]*net.TCPListener // original -> local relay port
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	work      chan struct{}
	tcp       map[uint16]uint16
	reply     map[uint16]uint16
	exchange  exchangeFunc
	dial      dialFunc
	closeDoH  func()
	log       func(string, ...any)
	sendErr   sync.Once
}

// Start protects the Windows resolver alongside either traffic-routing mode.
// There are no persistent DNS, adapter or policy changes. Closing the dynamic
// handles restores the previous path. The lower drop handle prevents
// plaintext fallbacks if the packet worker exits unexpectedly.
func Start(dllPath string, servers []string, bypass []doh.Flow, dial dialFunc, log func(string, ...any)) (*Session, error) {
	resolver := doh.New(dial)
	s, err := start(dllPath, servers, bypass, resolver.Exchange, dial, log)
	if err != nil {
		resolver.Close()
		return nil, err
	}
	s.closeDoH = resolver.Close
	if ok, _, _ := dnsFlush.Call(); ok == 0 {
		log("DNS 已经改走节点，Windows DNS 缓存清理失败")
	}
	return s, nil
}

func start(dllPath string, servers []string, bypass []doh.Flow, exchange exchangeFunc, dial dialFunc, log func(string, ...any)) (result *Session, err error) {
	d, err := loadDriver(dllPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{
		driver: d, ctx: ctx, cancel: cancel, exchange: exchange, dial: dial, log: log,
		work: make(chan struct{}, 128), listeners: make(map[uint16]*net.TCPListener),
		tcp: make(map[uint16]uint16), reply: make(map[uint16]uint16),
	}
	defer func() {
		if err != nil {
			s.Close()
		}
	}()
	for _, port := range []uint16{dnsPort, httpsPort, tlsPort} {
		ln, err := net.ListenTCP("tcp", &net.TCPAddr{Port: 0})
		if err != nil {
			return nil, err
		}
		local := uint16(ln.Addr().(*net.TCPAddr).Port)
		s.listeners[port], s.tcp[port], s.reply[local] = ln, local, port
	}
	primary, dropped, exposure := filters(servers, bypass, s.reply)
	s.portDrop, err = d.capture(exposure, 220, flagDrop)
	if err != nil {
		return nil, fmt.Errorf("隔离本机 DNS 中继：%w", err)
	}
	s.tailDrop, err = d.capture(dropped, 190, flagDrop)
	if err != nil {
		return nil, fmt.Errorf("阻止 DNS 直连回落：%w", err)
	}
	s.packet, err = d.capture(primary, 200, 0)
	if err != nil {
		return nil, fmt.Errorf("接管系统 DNS：%w", err)
	}
	for port, ln := range s.listeners {
		s.wg.Go(func() { s.accept(ln, port) })
	}
	s.wg.Go(s.capture)
	return s, nil
}

func filters(servers []string, bypass []doh.Flow, ports map[uint16]uint16) (primary, dropped, exposure string) {
	// Also recognize the encrypted resolvers built into Windows. Adapter
	// addresses include privately configured recursive DNS servers.
	servers = append(slices.Clone(servers),
		"1.1.1.1", "1.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1001",
		"8.8.8.8", "8.8.4.4", "2001:4860:4860::8888", "2001:4860:4860::8844",
		"9.9.9.9", "149.112.112.112", "2620:fe::fe", "2620:fe::9")
	hosts := make(map[string]bool)
	var dnsHosts []string
	for _, text := range servers {
		ip, err := netip.ParseAddr(text)
		if err != nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		predicate := ipPredicate(ip)
		if !hosts[predicate] {
			dnsHosts = append(dnsHosts, predicate)
			hosts[predicate] = true
		}
	}
	dohHosts := "(" + strings.Join(dnsHosts, " or ") + ")"
	match := "(tcp.DstPort == 53 or udp.DstPort == 53 or (!loopback and (tcp.DstPort == 853 or (tcp.DstPort == 443 and " + dohHosts + "))))"
	block := "(udp.DstPort == 137 or udp.DstPort == 5353 or udp.DstPort == 5355 or tcp.DstPort == 5355 or udp.DstPort == 853 or (!loopback and udp.DstPort == 443 and " + dohHosts + "))"
	var except []string
	for _, flow := range bypass {
		if !flow.Endpoint.IsValid() {
			continue
		}
		proto := "tcp"
		if flow.UDP {
			proto = "udp"
		}
		except = append(except, fmt.Sprintf("(%s and %s.DstPort == %d)", ipPredicate(flow.Endpoint.Addr()), proto, flow.Endpoint.Port()))
	}
	var allow string
	if len(except) != 0 {
		allow = " and ((" + strings.Join(except, " or ") + ") ? false : true)"
	}
	var outboundPorts, inboundPorts []string
	for port := range ports {
		outboundPorts = append(outboundPorts, fmt.Sprintf("tcp.SrcPort == %d", port))
		inboundPorts = append(inboundPorts, fmt.Sprintf("tcp.DstPort == %d", port))
	}
	replies := strings.Join(outboundPorts, " or ")
	primary = "outbound and ((" + match + allow + ") or " + replies + ")"
	dropped = "outbound and (((" + match + " or " + block + ")" + allow + ") or " + replies + ")"
	// The relay listens on an ephemeral port to receive reflected packets.
	// Block actual remote or loopback connections to it at a higher priority
	// than the packet injector, which only feeds the lower-priority stack.
	exposure = "(!outbound or loopback) and (" + strings.Join(inboundPorts, " or ") + ")"
	return
}

func ipPredicate(ip netip.Addr) string {
	ip = ip.Unmap().WithZone("")
	if ip.Is4() {
		return "ip.DstAddr == " + ip.String()
	}
	return "ipv6.DstAddr == " + ip.String()
}

func (s *Session) capture() {
	buf := make([]byte, 65575) // IPv6 header plus the maximum IP payload
	for s.ctx.Err() == nil {
		var addr address
		n, err := s.driver.recv(s.packet, buf, &addr)
		if err != nil {
			if s.ctx.Err() == nil {
				s.log("DNS 接管已中断，受保护的 DNS 连接会被阻止：%v", err)
			}
			return
		}
		if n < 20 {
			continue
		}
		packet := buf[:n]
		tcp, udp, body := s.driver.headers(packet)
		if tcp != 0 {
			s.reflect(packet, tcp, addr)
		} else if udp != 0 && len(body) >= 12 {
			s.enqueueUDP(packet, udp, body, addr)
		}
	}
}

func (s *Session) reflect(packet []byte, tcp int, addr address) {
	src := binary.BigEndian.Uint16(packet[tcp:])
	dst := binary.BigEndian.Uint16(packet[tcp+2:])
	port, reply := s.reply[src]
	if !reply {
		port = s.tcp[dst]
	}
	if port == 0 {
		return
	}
	loopback := addr.flags&flagLoopback != 0
	reflectTCP(packet, tcp, reply, loopback, port)
	if !loopback {
		addr.flags &^= flagOutbound
	}
	s.inject(packet, addr)
}

func (s *Session) enqueueUDP(packet []byte, udp int, body []byte, addr address) {
	select {
	case s.work <- struct{}{}:
	default:
		return
	}
	// Recv reuses the packet buffer; retain only the header and query.
	header := slices.Clone(packet[:udp+8])
	query := slices.Clone(body)
	s.wg.Go(func() {
		defer func() { <-s.work }()
		reply := answer(s.ctx, s.exchange, query, true)
		if len(reply) == 0 || s.ctx.Err() != nil {
			return
		}
		if addr.flags&flagLoopback == 0 {
			addr.flags &^= flagOutbound
		}
		s.inject(udpResponse(header, udp, reply), addr)
	})
}

func (s *Session) inject(packet []byte, addr address) {
	if err := s.driver.inject(s.packet, packet, &addr, true); err != nil && s.ctx.Err() == nil {
		s.sendErr.Do(func() { s.log("DNS 应答注入失败：%v", err) })
	}
}

func (s *Session) accept(ln *net.TCPListener, port uint16) {
	for {
		client, err := ln.AcceptTCP()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log("DNS 中继监听失败：%v", err)
			}
			return
		}
		select {
		case s.work <- struct{}{}:
			s.wg.Go(func() {
				defer func() { <-s.work }()
				defer client.Close()
				stop := context.AfterFunc(s.ctx, func() { client.Close() })
				defer stop()
				if port == dnsPort {
					serveDNSStream(s.ctx, client, s.exchange)
				} else {
					s.relayEncrypted(client, port)
				}
			})
		default:
			client.Close()
		}
	}
}

func (s *Session) relayEncrypted(client *net.TCPConn, port uint16) {
	host := client.RemoteAddr().(*net.TCPAddr).IP.String()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	up, err := s.dial(ctx, host, int(port))
	cancel()
	if err != nil {
		return
	}
	defer up.Close()
	stop := context.AfterFunc(s.ctx, func() { up.Close() })
	defer stop()
	deadline := time.Now().Add(2 * time.Minute)
	_ = up.SetDeadline(deadline)
	_ = client.SetDeadline(deadline)
	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(up, client)
		if tcp, ok := up.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
	}()
	io.Copy(client, up)
	client.Close()
	up.Close()
	<-done
}

func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		if s.packet != 0 {
			s.driver.shutdown.Call(s.packet, 1) // WINDIVERT_SHUTDOWN_RECV
		}
		for _, ln := range s.listeners {
			ln.Close()
		}
		s.wg.Wait()
		for _, h := range []uintptr{s.packet, s.tailDrop, s.portDrop} {
			if h != 0 {
				s.driver.close.Call(h)
			}
		}
		if s.closeDoH != nil {
			s.closeDoH()
		}
		_ = s.driver.library.Release()
	})
	return nil
}
