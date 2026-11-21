// Package doh exchanges DNS wire messages over authenticated HTTPS. Both the
// node bootstrap and the session relay use a numeric endpoint, so neither
// needs to look up the resolver through Windows DNS.
package doh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Flow identifies a node endpoint that may use a DNS transport port.
type Flow struct {
	Endpoint netip.AddrPort
	UDP      bool
}

const (
	Address    = "1.1.1.1"
	endpoint   = "https://cloudflare-dns.com/dns-query"
	wireType   = "application/dns-message"
	maxMessage = 65535
)

type Resolver struct {
	client    *http.Client
	transport *http.Transport
}

// New uses a direct numeric HTTPS connection when dial is nil. During an
// acceleration session the caller supplies a dialer through the node.
func New(dial func(context.Context, string, int) (net.Conn, error)) *Resolver {
	d := &net.Dialer{Timeout: 4 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if dial != nil {
				return dial(ctx, Address, 443)
			}
			return d.DialContext(ctx, "tcp4", Address+":443")
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 4 * time.Second,
		IdleConnTimeout:     30 * time.Second,
		MaxConnsPerHost:     16,
	}
	return &Resolver{
		transport: tr,
		client: &http.Client{
			Transport: tr,
			Timeout:   5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (r *Resolver) Close() { r.transport.CloseIdleConnections() }

// Exchange validates the DNS request and reply, including the transaction ID
// and question. Nothing falls back to a system or plaintext resolver.
func (r *Resolver) Exchange(ctx context.Context, wire []byte) ([]byte, error) {
	var query dnsmessage.Message
	if err := query.Unpack(wire); err != nil || query.Header.Response || len(query.Questions) == 0 {
		return nil, errors.New("无效 DNS 请求")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", wireType)
	req.Header.Set("Accept", wireType)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.EqualFold(resp.Header.Get("Content-Type"), wireType) {
		return nil, fmt.Errorf("DoH 响应无效（HTTP %d）", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+1))
	if err != nil {
		return nil, err
	}
	var reply dnsmessage.Message
	if len(body) > maxMessage || reply.Unpack(body) != nil || !reply.Header.Response ||
		reply.Header.ID != query.Header.ID || len(reply.Questions) != len(query.Questions) {
		return nil, errors.New("DNS 应答与请求不匹配")
	}
	for i, q := range query.Questions {
		p := reply.Questions[i]
		if !strings.EqualFold(q.Name.String(), p.Name.String()) || q.Type != p.Type || q.Class != p.Class {
			return nil, errors.New("DNS 应答与请求不匹配")
		}
	}
	return body, nil
}

var bootstrap = New(nil)

// LookupBootstrap resolves a node or other connection bootstrap name over
// fixed-IP DoH. Exchange is wired through the node for subsequent system DNS.
func LookupBootstrap(ctx context.Context, network, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		switch network {
		case "ip4":
			return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
		case "ip6":
			return []net.IP{net.IPv6loopback}, nil
		}
		return []net.IP{net.IPv6loopback, net.IPv4(127, 0, 0, 1)}, nil
	}
	return bootstrap.LookupIP(ctx, network, host)
}

func (r *Resolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	name, err := dnsmessage.NewName(strings.TrimSuffix(host, ".") + ".")
	if err != nil {
		return nil, err
	}
	types := []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA}
	switch network {
	case "ip4":
		types = types[:1]
	case "ip6":
		types = types[1:]
	}
	var ips []net.IP
	var lastErr error
	for _, typ := range types {
		query := dnsmessage.Message{
			Header:    dnsmessage.Header{RecursionDesired: true},
			Questions: []dnsmessage.Question{{Name: name, Type: typ, Class: dnsmessage.ClassINET}},
		}
		wire, err := query.Pack()
		if err != nil {
			return nil, err
		}
		body, err := r.Exchange(ctx, wire)
		if err != nil {
			lastErr = err
			continue
		}
		var reply dnsmessage.Message
		_ = reply.Unpack(body) // Exchange has already validated the message.
		for _, answer := range reply.Answers {
			switch addr := answer.Body.(type) {
			case *dnsmessage.AResource:
				ips = append(ips, net.IP(addr.A[:]))
			case *dnsmessage.AAAAResource:
				ips = append(ips, net.IP(addr.AAAA[:]))
			}
		}
	}
	if len(ips) != 0 {
		return ips, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, &net.DNSError{Name: host, Err: "没有可用的地址", IsNotFound: true}
}
