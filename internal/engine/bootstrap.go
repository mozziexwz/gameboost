package engine

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	apicommon "github.com/enfein/mieru/v3/apis/common"
	"github.com/enfein/mieru/v3/pkg/appctl/appctlpb"
	"google.golang.org/protobuf/proto"

	"github.com/mozziexwz/gameboost/internal/doh"
)

// prepareNode resolves bootstrap names before the session can intercept any
// DNS. Copy the endpoints so reconnects keep using IPs until the next start.
func prepareNode(original *appctlpb.ClientProfile) (*appctlpb.ClientProfile, []doh.Flow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	profile := proto.Clone(original).(*appctlpb.ClientProfile)
	var servers []*appctlpb.ServerEndpoint
	var flows []doh.Flow
	var lastErr error

	for _, server := range profile.GetServers() {
		host := server.GetDomainName()
		if host == "" {
			host = server.GetIpAddress()
		}
		ips, err := doh.LookupBootstrap(ctx, "ip", host)
		if err != nil {
			lastErr = fmt.Errorf("节点域名 %s 的加密解析失败：%w", host, err)
			continue
		}
		for _, ip := range ips {
			addr, ok := netip.AddrFromSlice(ip)
			if !ok {
				continue
			}
			copy := proto.Clone(server).(*appctlpb.ServerEndpoint)
			copy.DomainName = nil
			copy.IpAddress = proto.String(addr.Unmap().String())
			servers = append(servers, copy)
			for _, binding := range copy.GetPortBindings() {
				for _, port := range [...]uint16{53, 137, 443, 853, 5353, 5355} {
					if coversPort(binding, int(port)) {
						flows = append(flows, doh.Flow{
							Endpoint: netip.AddrPortFrom(addr.Unmap(), port),
							UDP:      binding.GetProtocol() == appctlpb.TransportProtocol_UDP,
						})
					}
				}
			}
		}
	}
	if len(servers) == 0 {
		if lastErr != nil {
			return nil, nil, lastErr
		}
		return nil, nil, fmt.Errorf("节点配置没有可用的地址")
	}
	profile.Servers = servers
	if d := profile.GetDialer(); d != nil {
		ips, err := doh.LookupBootstrap(ctx, "ip", d.GetHost())
		if err != nil {
			return nil, nil, fmt.Errorf("节点前置代理的加密解析失败：%w", err)
		}
		addr, _ := netip.AddrFromSlice(ips[0])
		addr = addr.Unmap()
		d.Host = proto.String(addr.String())
		flows = append(flows, doh.Flow{Endpoint: netip.AddrPortFrom(addr, uint16(d.GetPort()))})
	}
	return profile, flows, nil
}

func coversPort(b *appctlpb.PortBinding, port int) bool {
	if int(b.GetPort()) == port {
		return true
	}
	lo, hi, found := strings.Cut(b.GetPortRange(), "-")
	if !found {
		return false
	}
	first, _ := strconv.Atoi(lo)
	last, _ := strconv.Atoi(hi)
	return first <= port && port <= last
}

// Runtime destinations in SOCKS requests resolve on the server. Refuse any
// client-side lookup inside the established node to avoid a DNS fallback.
type noLocalDNS = apicommon.NilDNSResolver
