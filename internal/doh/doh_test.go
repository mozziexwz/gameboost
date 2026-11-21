package doh

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSOverHTTPSUsesOnlyTheSuppliedTunnel(t *testing.T) {
	var outage atomic.Bool
	var queries atomic.Int32
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || req.URL.Path != "/dns-query" ||
			req.Header.Get("Content-Type") != wireType {
			t.Errorf("unexpected DoH request: %s %s", req.Method, req.URL.Path)
		}
		queries.Add(1)
		if outage.Load() {
			http.Redirect(w, req, "http://127.0.0.1:53/", http.StatusMovedPermanently)
			return
		}
		buf, err := io.ReadAll(req.Body)
		var message dnsmessage.Message
		if err != nil || message.Unpack(buf) != nil || len(message.Questions) != 1 {
			t.Errorf("invalid DNS wire format: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		message.Header.Response = true
		body, _ := message.Pack()
		w.Header().Set("Content-Type", wireType)
		w.Write(body)
	}))
	defer tlsServer.Close()
	resolver := New(func(ctx context.Context, ip string, port int) (net.Conn, error) {
		if got := net.JoinHostPort(ip, strconv.Itoa(port)); got != "1.1.1.1:443" {
			t.Errorf("unexpected tunnel destination: %s", got)
		}
		dialer := net.Dialer{}
		return dialer.DialContext(ctx, "tcp", tlsServer.Listener.Addr().String())
	})
	defer resolver.Close()
	roots := x509.NewCertPool()
	roots.AddCert(tlsServer.Certificate())
	resolver.transport.TLSClientConfig = &tls.Config{
		RootCAs: roots, ServerName: "example.com",
	}
	query := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 17, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("gameboost.test."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	wire, _ := query.Pack()
	if _, err := resolver.Exchange(context.Background(), wire); err != nil {
		t.Fatal(err)
	}
	outage.Store(true)
	if _, err := resolver.Exchange(context.Background(), wire); err == nil || queries.Load() != 2 {
		t.Fatalf("unexpected fallback result %v, HTTPS requests %d", err, queries.Load())
	}
}
