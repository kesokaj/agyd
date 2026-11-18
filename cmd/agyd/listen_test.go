package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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

type stubListener struct{ net.Listener }

func (stubListener) Close() error { return nil }

func TestListenServeFallback(t *testing.T) {
	t.Run("candidatePorts uses 8080 then 3000 when unset, single port when explicit", func(t *testing.T) {
		if got := candidatePorts(appConfig{Host: "127.0.0.1", Port: 8080, PortExplicit: false}); !slices.Equal(got, []int{8080, 3000}) {
			t.Errorf("unset candidatePorts = %v, want [8080 3000]", got)
		}
		if got := candidatePorts(appConfig{Host: "127.0.0.1", Port: 8080, PortExplicit: true}); !slices.Equal(got, []int{8080}) {
			t.Errorf("explicit 8080 candidatePorts = %v, want [8080]", got)
		}
		if got := candidatePorts(appConfig{Host: "127.0.0.1", Port: 9090, PortExplicit: true}); !slices.Equal(got, []int{9090}) {
			t.Errorf("explicit 9090 candidatePorts = %v, want [9090]", got)
		}
	})

	t.Run("8080 free binds 8080 without touching 3000", func(t *testing.T) {
		var tried []string
		ln, port, err := listenServe(appConfig{Host: "127.0.0.1", Port: 8080}, func(netw, addr string) (net.Listener, error) {
			tried = append(tried, netw+":"+addr)
			return stubListener{}, nil
		})
		if err != nil || port != 8080 {
			t.Fatalf("listenServe = %d, %v; want 8080, nil", port, err)
		}
		_ = ln.Close()
		if !slices.Equal(tried, []string{"tcp4:127.0.0.1:8080"}) {
			t.Errorf("tried = %v, want [tcp4:127.0.0.1:8080]", tried)
		}
	})

	t.Run("8080 held falls back to 3000", func(t *testing.T) {
		var tried []string
		ln, port, err := listenServe(appConfig{Host: "127.0.0.1", Port: 8080}, func(netw, addr string) (net.Listener, error) {
			tried = append(tried, addr)
			if addr == "127.0.0.1:8080" {
				return nil, fmt.Errorf("bind: %w", syscall.EADDRINUSE)
			}
			return stubListener{}, nil
		})
		if err != nil || port != 3000 {
			t.Fatalf("listenServe = %d, %v; want 3000, nil", port, err)
		}
		_ = ln.Close()
		if !slices.Equal(tried, []string{"127.0.0.1:8080", "127.0.0.1:3000"}) {
			t.Errorf("tried = %v, want [127.0.0.1:8080 127.0.0.1:3000]", tried)
		}
	})

	t.Run("both 8080 and 3000 held fails naming 8080, 3000, and -port", func(t *testing.T) {
		_, _, err := listenServe(appConfig{Host: "127.0.0.1", Port: 8080}, func(_, _ string) (net.Listener, error) {
			return nil, fmt.Errorf("bind: %w", syscall.EADDRINUSE)
		})
		if err == nil || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("err = %v, want wrapping EADDRINUSE", err)
		}
		for _, want := range []string{"8080", "3000", "-port"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err %q missing %q", err.Error(), want)
			}
		}
	})

	t.Run("explicit port held fails with no fallback", func(t *testing.T) {
		var tried []string
		_, _, err := listenServe(appConfig{Host: "127.0.0.1", Port: 8080, PortExplicit: true}, func(_, addr string) (net.Listener, error) {
			tried = append(tried, addr)
			return nil, fmt.Errorf("bind: %w", syscall.EADDRINUSE)
		})
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("err = %v, want EADDRINUSE", err)
		}
		if !slices.Equal(tried, []string{"127.0.0.1:8080"}) {
			t.Errorf("explicit port tried %v, want [127.0.0.1:8080] only", tried)
		}
	})

	t.Run("non-EADDRINUSE error on 8080 is not masked by fallback", func(t *testing.T) {
		var tried []string
		_, _, err := listenServe(appConfig{Host: "127.0.0.1", Port: 8080}, func(_, addr string) (net.Listener, error) {
			tried = append(tried, addr)
			return nil, fmt.Errorf("bind: %w", syscall.EADDRNOTAVAIL)
		})
		if !errors.Is(err, syscall.EADDRNOTAVAIL) {
			t.Fatalf("err = %v, want EADDRNOTAVAIL", err)
		}
		if !slices.Equal(tried, []string{"127.0.0.1:8080"}) {
			t.Errorf("non-EADDRINUSE tried %v, want [127.0.0.1:8080] only", tried)
		}
	})

	t.Run("real TCP listeners: first held falls back to second, both held fails", func(t *testing.T) {
		held1, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer held1.Close()
		p1 := held1.Addr().(*net.TCPAddr).Port

		p2 := freeLoopbackPort(t)
		ln, gotPort, err := listenFirst("127.0.0.1", []int{p1, p2}, net.Listen)
		if err != nil || gotPort != p2 {
			t.Fatalf("listenFirst([%d, %d]) = %d, %v; want %d, nil", p1, p2, gotPort, err, p2)
		}
		_ = ln.Close()

		held2, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer held2.Close()
		p2Held := held2.Addr().(*net.TCPAddr).Port

		if _, _, err := listenFirst("127.0.0.1", []int{p1, p2Held}, net.Listen); err == nil ||
			!strings.Contains(err.Error(), strconv.Itoa(p1)) ||
			!strings.Contains(err.Error(), strconv.Itoa(p2Held)) ||
			!strings.Contains(err.Error(), "-port") {
			t.Fatalf("both held err = %v, want naming %d, %d, and -port", err, p1, p2Held)
		}
	})
}

func TestServeExplicitPortInUse(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	busyPort := strconv.Itoa(held.Addr().(*net.TCPAddr).Port)

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ws := realTempDir(t)

	for _, tc := range []struct {
		name      string
		serveArgs string
		envPort   string
	}{
		{name: "explicit -port flag", serveArgs: "-port " + busyPort},
		{name: "explicit AGYD_PORT env", envPort: busyPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := filepath.Join(realTempDir(t), "state")
			storePath := filepath.Join(realTempDir(t), "acp-store.json")
			cmd := exec.Command(exe, "-test.run=^TestHelperServeAndACP$")
			cmd.Dir = ws
			cmd.Stdin = nil
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"AGYD_TEST_ROLE=serve",
				"AGYD_TEST_SERVE_ARGS=" + tc.serveArgs,
				"AGYD_TEST_ACP_STORE=" + storePath,
				"AGYD_AGY=" + exe,
				"AGYD_ACP_ARGS=-test.run=^TestHelperServeAndACP$ fake-acp",
				"AGYD_STATE_DIR=" + stateDir,
				"AGYD_WORKSPACE=" + ws,
			}
			if tc.envPort != "" {
				cmd.Env = append(cmd.Env, "AGYD_PORT="+tc.envPort)
			}
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected non-zero exit when port %s is held, got exit 0:\n%s", busyPort, out)
			}
			if !strings.Contains(string(out), "127.0.0.1:"+busyPort) {
				t.Errorf("output missing held address 127.0.0.1:%s:\n%s", busyPort, out)
			}
			// Port conflict must fail before touching state.json or spawning a session.
			if _, statErr := os.Stat(filepath.Join(stateDir, "state.json")); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("port conflict created or touched state.json (stat err = %v)", statErr)
			}
		})
	}
}

func TestRunLogsFallbackAndNonLoopbackWarningsAtBind(t *testing.T) {
	ws := realTempDir(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	baseCfg := appConfig{
		Host:             "127.0.0.1",
		Port:             8080,
		ACPBin:           exe,
		ACPArgs:          []string{"-test.run=^$"},
		Workspace:        ws,
		StateDir:         filepath.Join(realTempDir(t), "state"),
		PermissionPolicy: "allow_all",
		NoPrompt:         true,
	}

	hasWarnLine := func(logs, msgSubstr string) bool {
		for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
			if strings.Contains(line, `"severity":"WARNING"`) && strings.Contains(line, msgSubstr) {
				return true
			}
		}
		return false
	}

	t.Run("default port fallback logs WARNING", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var buf bytes.Buffer
		cfg := baseCfg
		err := runWithListen(ctx, newLogger(&buf), cfg, func(_, addr string) (net.Listener, error) {
			if addr == "127.0.0.1:8080" {
				return nil, &net.OpError{Op: "listen", Net: "tcp4", Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE}}
			}
			return stubListener{}, nil
		})
		if err != nil {
			t.Fatalf("runWithListen err = %v", err)
		}
		logs := buf.String()
		if !hasWarnLine(logs, `"message":"default port in use, using fallback"`) {
			t.Errorf("expected WARNING line for default port fallback, got:\n%s", logs)
		}
	})

	t.Run("non-loopback warning logs immediately on bind before startup completes", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var buf bytes.Buffer
		cfg := baseCfg
		cfg.Host = "0.0.0.0"
		cfg.PortExplicit = true
		err := runWithListen(ctx, newLogger(&buf), cfg, func(_, _ string) (net.Listener, error) {
			return stubListener{}, nil
		})
		if err != nil {
			t.Fatalf("runWithListen err = %v", err)
		}
		logs := buf.String()
		firstLine, _, _ := strings.Cut(strings.TrimSpace(logs), "\n")
		if !hasWarnLine(firstLine, `"message":"listening beyond loopback`) {
			t.Errorf("expected non-loopback WARNING as first log line immediately at bind, got:\n%s", logs)
		}
	})
}
