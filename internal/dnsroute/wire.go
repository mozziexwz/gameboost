package dnsroute

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

type exchangeFunc func(context.Context, []byte) ([]byte, error)

func answer(ctx context.Context, exchange exchangeFunc, request []byte, udp bool) []byte {
	var q dnsmessage.Message
	if err := q.Unpack(request); err != nil || q.Header.Response || len(q.Questions) == 0 {
		return nil
	}
	reply, err := exchange(ctx, request)
	if err == nil && (!udp || len(reply) <= udpLimit(q)) {
		return reply
	}
	// Preserve the question on failure and ask UDP clients to retry large
	// answers over TCP. Never send the original request to its nameserver.
	m := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:                 q.Header.ID,
			Response:           true,
			OpCode:             q.Header.OpCode,
			RecursionDesired:   q.Header.RecursionDesired,
			RecursionAvailable: true,
			Truncated:          err == nil,
		},
		Questions: q.Questions,
	}
	if err != nil {
		m.Header.RCode = dnsmessage.RCodeServerFailure
	}
	wire, _ := m.Pack()
	return wire
}

func udpLimit(q dnsmessage.Message) int {
	for _, r := range q.Additionals {
		if r.Header.Type == dnsmessage.TypeOPT {
			// Fit the IPv6 minimum MTU after IP and UDP headers.
			return min(max(int(r.Header.Class), 512), 1232)
		}
	}
	return 512
}

func serveDNSStream(ctx context.Context, conn net.Conn, exchange exchangeFunc) {
	var size [2]byte
	for {
		_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
		if _, err := io.ReadFull(conn, size[:]); err != nil {
			return
		}
		request := make([]byte, binary.BigEndian.Uint16(size[:]))
		if _, err := io.ReadFull(conn, request); err != nil {
			return
		}
		reply := answer(ctx, exchange, request, false)
		if len(reply) == 0 || len(reply) > 65535 {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(reply)))
		if _, err := conn.Write(append(size[:], reply...)); err != nil {
			return
		}
	}
}
