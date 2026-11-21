package probe

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestConfirmRelayThroughSOCKS5(t *testing.T) {
	s := startFakeSOCKS(t, "user", "secret", func(int, net.Conn) {})
	if err := ConfirmRelay(context.Background(), s.proxy(), "probe.invalid", 443); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.targets.Load("probe.invalid:443"); !ok {
		t.Fatal("missing SOCKS5 CONNECT to the probe target")
	}

	wrong := s.proxy()
	wrong.Password = "wrong"
	if err := ConfirmRelay(context.Background(), wrong, "probe.invalid", 443); err == nil || !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("SOCKS5 authentication failure missing: %v", err)
	}
}

func TestConfirmRelayAcceptsEarlySuccessAndTargetRefusal(t *testing.T) {
	for _, reply := range []byte{0, 5} {
		p := Proxy{DialNode: func(context.Context) (net.Conn, error) {
			local, node := net.Pipe()
			go func() {
				defer node.Close()
				if _, err := io.ReadFull(node, make([]byte, 10)); err == nil {
					_, _ = node.Write([]byte{5, reply, 0, 1, 0, 0, 0, 0, 0, 0})
				}
			}()
			return local, nil
		}}
		if err := ConfirmRelay(context.Background(), p, "192.0.2.25", 8443); err != nil {
			t.Fatalf("reply %d should confirm the node is reachable: %v", reply, err)
		}
	}
}

func TestConfirmRelayTimesOutOnIncompleteReply(t *testing.T) {
	p := Proxy{DialNode: func(context.Context) (net.Conn, error) {
		local, node := net.Pipe()
		go func() {
			defer node.Close()
			if _, err := io.ReadFull(node, make([]byte, 10)); err == nil {
				_, _ = node.Write([]byte{5, 0, 0, 1})
				_, _ = io.Copy(io.Discard, node)
			}
		}()
		return local, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := ConfirmRelay(ctx, p, "192.0.2.25", 8443); err == nil || !isTimeout(err) {
		t.Fatalf("partial CONNECT reply did not time out: %v", err)
	}
}
