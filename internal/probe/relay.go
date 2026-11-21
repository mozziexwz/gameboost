package probe

import (
	"context"
	"errors"
	"net"
	"time"
)

// ConfirmRelay checks if the remote node answers a CONNECT request. Both
// success and a SOCKS5 rejection confirm the tunnel reaches the node.
func ConfirmRelay(ctx context.Context, p Proxy, host string, port int) error {
	attempt, cancel := context.WithTimeout(ctx, AttemptTimeout)
	defer cancel()
	deadline, _ := attempt.Deadline()
	conn, err := dialRelayProbe(attempt, p, host, port, deadline)
	if err == nil {
		conn.Close()
		return nil
	}
	var reply connectReplyError
	if errors.As(err, &reply) && reply >= 1 && reply <= 8 {
		return nil
	}
	return err
}

func dialRelayProbe(ctx context.Context, p Proxy, host string, port int, deadline time.Time) (net.Conn, error) {
	if p.DialNode == nil {
		return DialSOCKS5Context(ctx, p, host, port, deadline)
	}
	conn, err := p.DialNode(ctx)
	if err != nil {
		return nil, err
	}
	return completeSOCKS5(ctx, conn, deadline, func() error {
		stream := mieruProbeStream{Conn: conn, deadline: deadline}
		return socks5RequestConnect(stream, host, port)
	})
}

// Mieru clears the read deadline after each Read. Keep one absolute deadline
// for the entire SOCKS5 reply, including fragments read by io.ReadFull.
type mieruProbeStream struct {
	net.Conn
	deadline time.Time
}

func (s mieruProbeStream) Read(b []byte) (int, error) {
	if err := s.SetReadDeadline(s.deadline); err != nil {
		return 0, err
	}
	return s.Conn.Read(b)
}
