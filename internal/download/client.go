package download

import (
	"context"
	"net"
	"time"
)

// dialer 封装连接超时、IP 版本与出口网卡设置。
type dialer struct {
	timeout time.Duration
	ipv     int    // 0=自动, 4=仅 IPv4, 6=仅 IPv6
	iface   string // 出口网卡名（--interface）
}

func (d *dialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	nd := &net.Dialer{Timeout: d.timeout, KeepAlive: 30 * time.Second}
	switch d.ipv {
	case 4:
		network = "tcp4"
	case 6:
		network = "tcp6"
	}
	if d.iface != "" {
		if la := interfaceLocalAddr(d.iface, d.ipv); la != nil {
			nd.LocalAddr = la
		}
	}
	return nd.DialContext(ctx, network, addr)
}

// interfaceLocalAddr 取指定网卡上第一个可用的本机地址。
func interfaceLocalAddr(name string, ipv int) net.Addr {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipn.IP.To4()
		switch {
		case ipv == 4:
			if ip4 == nil {
				continue
			}
			return &net.TCPAddr{IP: ip4}
		case ipv == 6:
			if ip4 != nil {
				continue
			}
			return &net.TCPAddr{IP: ipn.IP}
		case ip4 != nil:
			return &net.TCPAddr{IP: ip4}
		}
	}
	return nil
}
