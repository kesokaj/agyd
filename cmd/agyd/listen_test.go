package main

import (
	"errors"
	"net"
	"slices"
	"strconv"
	"testing"
)

func TestExposedHosts(t *testing.T) {
	ifaces := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
			&net.IPNet{IP: net.ParseIP("192.168.1.5"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("192.168.1.5"), Mask: net.CIDRMask(24, 32)},
			&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
			&net.IPNet{IP: net.ParseIP("fd00::5"), Mask: net.CIDRMask(64, 128)},
		}, nil
	}
	hostname := func() (string, error) { return "MyMac.local", nil }
	fqdn := func() (string, error) { return "mymac.corp.example", nil }
	bare := func() (string, error) { return "mymac", nil }
	noHostname := func() (string, error) { return "", errors.New("no hostname") }

	tests := []struct {
		name, host string
		hostname   func() (string, error)
		want       []string
	}{
		{"loopback adds its canonical IP", "127.0.0.1", hostname, []string{"127.0.0.1"}},
		{"other loopback IP is allowed", "127.0.0.2", hostname, []string{"127.0.0.2"}},
		{"mapped loopback is canonicalized", "::ffff:127.0.0.1", hostname, []string{"127.0.0.1"}},
		{"localhost adds nothing", "localhost", hostname, nil},
		{"ipv4 wildcard adds deduped non-link-local IPv4 IPs and hostname", "0.0.0.0", hostname, []string{"192.168.1.5", "mymac.local", "mymac"}},
		{"ipv6 wildcard adds only IPv6 interface IPs", "::", noHostname, []string{"fd00::5"}},
		{"specific IP adds only itself and hostname", "192.168.1.5", bare, []string{"192.168.1.5", "mymac", "mymac.local"}},
		{"non-canonical IPv6 is canonicalized", "fd00:0:0:0::5", noHostname, []string{"fd00::5"}},
		{"dotted hostname gets no .local suffix", "192.168.1.5", fqdn, []string{"192.168.1.5", "mymac.corp.example"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := exposedHosts(tt.host, ifaces, tt.hostname)
			if !slices.Equal(got, tt.want) {
				t.Errorf("exposedHosts(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestLocalURL(t *testing.T) {
	for host, want := range map[string]string{
		"0.0.0.0":     "http://127.0.0.1:8080",
		"::":          "http://[::1]:8080",
		"::1":         "http://[::1]:8080",
		"192.168.1.5": "http://192.168.1.5:8080",
	} {
		if got := localURL(appConfig{Host: host, Port: 8080}); got != want {
			t.Errorf("localURL(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestListenNetwork(t *testing.T) {
	for host, want := range map[string]string{
		"localhost":        "tcp",
		"127.0.0.1":        "tcp4",
		"0.0.0.0":          "tcp4",
		"::ffff:127.0.0.1": "tcp4",
		"::":               "tcp6",
		"::1":              "tcp6",
		"fd00::5":          "tcp6",
	} {
		if got := listenNetwork(host); got != want {
			t.Errorf("listenNetwork(%q) = %q, want %q", host, got, want)
		}
	}
}

// TestWildcardV4DoesNotListenOnV6 proves the dual-stack leak is closed: a
// 0.0.0.0 bind must refuse connections on ::1.
func TestWildcardV4DoesNotListenOnV6(t *testing.T) {
	ln, err := net.Listen(listenNetwork("0.0.0.0"), "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if c, err := net.Dial("tcp6", net.JoinHostPort("::1", strconv.Itoa(port))); err == nil {
		c.Close()
		t.Fatalf("dial [::1]:%d succeeded on a 0.0.0.0 bind, want refused", port)
	}
}
