package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPResponseLatencyUsesTheSelectedProxyAndReuse(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			time.Sleep(80 * time.Millisecond)
		}
		if r.URL.Path != "/probe" || r.URL.RawQuery != "sample=1" {
			t.Errorf("request sent to unexpected URL %s", r.URL)
		}
		w.Header().Set("Location", "http://redirect.invalid/")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	socks := startFakeSOCKS(t, "user", "fixture", pipeTo(origin.Listener.Addr().String()))
	r := HTTPResponseLatency(context.Background(), socks.proxy(), "http://probe.invalid:8080/probe?sample=1")
	_, proxied := socks.targets.Load("probe.invalid:8080")
	if requests.Load() != 2 || socks.sessions.Load() != 1 || !proxied || r.SuccessCount != 2 {
		t.Fatalf("wanted two HTTP responses on one proxied connection: %+v, requests=%d, connections=%d", r, requests.Load(), socks.sessions.Load())
	}
	if first, second := r.Samples[0].Duration, r.Samples[1].Duration; first <= second || r.displayLatency() != second {
		t.Fatalf("wanted the fastest response: first %v, second %v, result %v", first, second, r.displayLatency())
	}
}

func TestHTTPResponseLatencyRequiresAResponse(t *testing.T) {
	socks := startFakeSOCKS(t, "user", "fixture", func(int, net.Conn) {})
	r := HTTPResponseLatency(context.Background(), socks.proxy(), "http://probe.invalid:8080/probe")
	if r.SuccessCount != 0 || len(r.Errors) == 0 {
		t.Fatalf("SOCKS success alone cannot report an HTTP response: %+v", r)
	}
}
