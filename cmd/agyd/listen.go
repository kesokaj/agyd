package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"syscall"
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

var defaultServePorts = []int{8080, 3000}

// candidatePorts returns the port(s) agyd attempts to bind: the single
// requested port when -port or AGYD_PORT is set, else 8080 then 3000 (ADR-006).
func candidatePorts(cfg appConfig) []int {
	if cfg.PortExplicit {
		return []int{cfg.Port}
	}
	return defaultServePorts
}

func listenServe(cfg appConfig, listen func(network, address string) (net.Listener, error)) (net.Listener, int, error) {
	return listenFirst(cfg.Host, candidatePorts(cfg), listen)
}

// listenFirst binds the first free port in ports directly (no pre-check race).
// Only EADDRINUSE advances to a fallback port; if every candidate is in use and
// fallback was enabled, the error names all tried ports and -port.
func listenFirst(host string, ports []int, listen func(network, address string) (net.Listener, error)) (net.Listener, int, error) {
	netw := listenNetwork(host)
	for i, port := range ports {
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		ln, err := listen(netw, addr)
		if err == nil {
			return ln, port, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) || i == len(ports)-1 {
			if len(ports) > 1 && errors.Is(err, syscall.EADDRINUSE) {
				parts := make([]string, len(ports))
				for j, p := range ports {
					parts[j] = strconv.Itoa(p)
				}
				return nil, 0, fmt.Errorf("ports %s on %s are in use; pass -port (or set AGYD_PORT) to choose another port: %w",
					strings.Join(parts, " and "), host, err)
			}
			return nil, 0, fmt.Errorf("listening on %s: %w", addr, err)
		}
	}
	return nil, 0, errors.New("no listen ports configured")
}
