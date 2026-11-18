package main

import (
	"net"
	"strconv"
	"strings"
)

// isLoopbackHost reports whether host is localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// exposedHosts returns the extra Host header names the guard accepts for a
// non-loopback bind: the bound IP itself, or every non-link-local interface
// IP of the wildcard's family (0.0.0.0: IPv4 only, ::: IPv6 only, matching
// listenNetwork), plus the machine's hostname (and its .local form for a
// bare name). A loopback bind adds only its canonical IP (e.g. 127.0.0.2),
// since the guard's built-in names cover just localhost, 127.0.0.1 and ::1.
// IPs are returned in canonical form to match Guard.
func exposedHosts(host string, ifaceAddrs func() ([]net.Addr, error), hostname func() (string, error)) []string {
	if isLoopbackHost(host) {
		if ip := net.ParseIP(host); ip != nil {
			return []string{ip.String()}
		}
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(h string) {
		if h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		wantV4 := ip.To4() != nil
		addrs, _ := ifaceAddrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() && (n.IP.To4() != nil) == wantV4 {
				add(n.IP.String())
			}
		}
	} else if ip != nil {
		add(ip.String())
	} else {
		add(host)
	}
	if name, err := hostname(); err == nil && name != "" {
		name = strings.ToLower(name)
		add(name)
		switch {
		case strings.HasSuffix(name, ".local"):
			add(strings.TrimSuffix(name, ".local"))
		case !strings.Contains(name, "."):
			add(name + ".local")
		}
	}
	return out
}

// localURL is the address to print for opening the docs on this machine. A
// wildcard bind maps to the loopback address of the same family.
func localURL(cfg appConfig) string {
	host := cfg.Host
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
		if ip.To4() == nil {
			host = "::1"
		}
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(cfg.Port))
}

// listenNetwork pins the socket family to the -host literal: an IPv4 host
// (including 0.0.0.0) listens on tcp4 only, an IPv6 host on tcp6 only. Plain
// "tcp" would open 0.0.0.0 as a dual-stack socket that also accepts every
// IPv6 address, including globally routable ones. localhost stays "tcp".
func listenNetwork(host string) string {
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return "tcp"
	case ip.To4() != nil:
		return "tcp4"
	default:
		return "tcp6"
	}
}
