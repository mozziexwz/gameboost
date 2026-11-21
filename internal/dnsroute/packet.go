package dnsroute

import "encoding/binary"

const (
	dnsPort   = 53
	tlsPort   = 853
	httpsPort = 443
)

// addresses returns the source and destination fields in a parsed IP packet.
func addresses(packet []byte) (src, dst []byte) {
	if packet[0]>>4 == 6 {
		return packet[8:24], packet[24:40]
	}
	return packet[12:16], packet[16:20]
}

// reflectTCP follows WinDivert's streamdump model. For remote flows the local
// listener sees the original destination as its peer. For loopback only the
// port changes, since Windows exposes loopback packets on the outbound path.
func reflectTCP(packet []byte, tcp int, isReply, loopback bool, port uint16) {
	if !loopback {
		src, dst := addresses(packet)
		var tmp [16]byte
		copy(tmp[:], src)
		copy(src, dst)
		copy(dst, tmp[:])
	}
	offset := tcp + 2
	if isReply {
		offset = tcp
	}
	binary.BigEndian.PutUint16(packet[offset:], port)
}

func udpResponse(packet []byte, udp int, payload []byte) []byte {
	src, dst := addresses(packet)
	ipLength := 20
	if len(src) == 16 {
		ipLength = 40
	}
	out := make([]byte, ipLength+8+len(payload))
	if ipLength == 20 {
		out[0], out[8], out[9] = 0x45, 64, 17
		binary.BigEndian.PutUint16(out[2:], uint16(len(out)))
	} else {
		out[0], out[6], out[7] = 0x60, 17, 64
		binary.BigEndian.PutUint16(out[4:], uint16(len(out)-ipLength))
	}
	from, to := addresses(out)
	copy(from, dst)
	copy(to, src)
	copy(out[ipLength:], packet[udp+2:udp+4])
	copy(out[ipLength+2:], packet[udp:udp+2])
	binary.BigEndian.PutUint16(out[ipLength+4:], uint16(8+len(payload)))
	copy(out[ipLength+8:], payload)
	return out
}
