package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"
)

type RouteMode string

const (
	ModeProcess RouteMode = "process"
	ModeIP      RouteMode = "ip"
)

// RouteRule returns the active, validated route as ProxyBridge list fields.
// An empty active list is handled by the controller before adding a rule.
func (s Settings) RouteRule() (processes, hosts string) {
	if s.Mode == ModeIP {
		_, hosts, _ = parseTargets(s.Targets)
		return "*", hosts
	}
	return strings.Join(s.ProcessRules(), ";"), "*"
}

// ProcessRules returns the executable rules and the recursive image-path
// patterns for the configured folders. ProxyBridge's * spans separators.
func (s Settings) ProcessRules() []string {
	rules := make([]string, 0, len(s.Processes)+len(s.ProcessFolders))
	rules = append(rules, s.Processes...)
	for _, folder := range s.ProcessFolders {
		rules = append(rules, strings.TrimRight(folder, `\`)+`\*`)
	}
	return rules
}

func NormalizeProcessFolders(paths []string) ([]string, error) {
	folders := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, value := range paths {
		folder := strings.ReplaceAll(strings.TrimSpace(value), "/", `\`)
		if folder == "" {
			continue
		}
		if strings.ContainsAny(folder, ",;\"\x00\r\n\t*?<>|") || !isAbsoluteProcessFolder(folder) {
			return nil, fmt.Errorf("请输入有效的文件夹绝对路径：%q", value)
		}
		folder = filepath.Clean(folder)
		key := strings.ToLower(strings.TrimRight(folder, `\`))
		if !seen[key] {
			folders = append(folders, folder)
			seen[key] = true
		}
	}
	return folders, nil
}

func isAbsoluteProcessFolder(folder string) bool {
	if !filepath.IsAbs(folder) {
		return false
	}
	if networkPath, ok := strings.CutPrefix(folder, `\\`); ok {
		host, rest, _ := strings.Cut(networkPath, `\`)
		share, _, _ := strings.Cut(rest, `\`)
		return host != "" && host != "." && share != "" && share != "." && !strings.Contains(folder, ":")
	}
	drive := folder[0] | ' '
	return drive >= 'a' && drive <= 'z' && folder[1] == ':' && !strings.Contains(folder[2:], ":")
}

func NormalizeTargets(targets []string) ([]string, error) {
	normalized, _, err := parseTargets(targets)
	return normalized, err
}

func parseTargets(targets []string) ([]string, string, error) {
	normalized := make([]string, 0, len(targets))
	native := make([]string, 0, len(targets))
	seen := make(map[string]bool)
	for _, target := range targets {
		target = strings.TrimSpace(target)
		if target == "" {
			continue
		}
		display, pattern, err := ipRule(target)
		if err != nil {
			return nil, "", err
		}
		if seen[display] {
			continue
		}
		seen[display] = true
		normalized = append(normalized, display)
		native = append(native, pattern)
	}
	hosts := strings.Join(native, ";")
	// MAX_LIST_SIZE includes the terminator in the pinned Windows core.
	if len(hosts) >= 65536 {
		return nil, "", errors.New("目标 IP 清单过长，请合并网段或减少条目")
	}
	return normalized, hosts, nil
}

func ipRule(value string) (display, pattern string, err error) {
	if addr, err := netip.ParseAddr(value); err == nil && addr.Zone() == "" {
		text := addr.Unmap().String()
		return text, text, nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil || prefix.Addr().Is4In6() {
		return "", "", fmt.Errorf("请输入 IPv4、IPv6 或 CIDR 网段：%q", value)
	}
	prefix = prefix.Masked()
	addr := prefix.Addr()
	if prefix.Bits() == addr.BitLen() {
		return addr.String(), addr.String(), nil
	}
	if addr.Is6() {
		return prefix.String(), prefix.String(), nil
	}
	// ProxyBridge 4.1.0 for Windows accepts IPv4 ranges but only IPv6 CIDR.
	first := addr.As4()
	last := binary.BigEndian.Uint32(first[:]) | (uint32(1)<<(32-prefix.Bits()) - 1)
	var end [4]byte
	binary.BigEndian.PutUint32(end[:], last)
	return prefix.String(), addr.String() + "-" + netip.AddrFrom4(end).String(), nil
}
