package sshclient_test

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/sshclient"
	"github.com/zxhtom/portmap/internal/testrelay"
)

func portTestRelay(t *testing.T) config.Remote {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	r, err := testrelay.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	host, port, _ := net.SplitHostPort(r.Addr())
	n, _ := strconv.Atoi(port)
	return config.Remote{Name: "relay", Host: host, Port: n, User: "test", Auth: config.AuthConfig{Method: "password", Password: "test-only"}}
}
func TestAllocatePortChecksActualBindAndFindsFiveDigits(t *testing.T) {
	remote := portTestRelay(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	taken := occupied.Addr().(*net.TCPAddr).Port
	if _, err := sshclient.AllocatePort(remote, taken, func(int) bool { return false }); err == nil || !strings.Contains(err.Error(), "不可用") {
		t.Fatalf("occupied port accepted: %v", err)
	}
	selected, err := sshclient.AllocatePort(remote, 0, func(p int) bool { return p == taken })
	if err != nil {
		t.Fatal(err)
	}
	if selected < 10000 || selected > 65535 || selected == taken {
		t.Fatalf("invalid random port %d", selected)
	}
	// Probe must release its socket so the real tunnel can acquire it.
	ln, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(selected)))
	if err != nil {
		t.Fatalf("probe retained port: %v", err)
	}
	ln.Close()
	selectedAgain, err := sshclient.AllocatePort(remote, selected, func(int) bool { return false })
	if err != nil || selectedAgain != selected {
		t.Fatalf("fixed free port failed: %d %v", selectedAgain, err)
	}
}
func TestAllocateRespectsConfiguredReservationsAndAuthenticationErrors(t *testing.T) {
	remote := portTestRelay(t)
	if _, err := sshclient.AllocatePort(remote, 23456, func(int) bool { return true }); err == nil || !strings.Contains(err.Error(), "占用") {
		t.Fatalf("reserved fixed port accepted: %v", err)
	}
	if _, err := sshclient.AllocatePort(remote, 0, func(int) bool { return true }); err == nil {
		t.Fatal("exhausted range accepted")
	}
	remote.Auth = config.AuthConfig{Method: "key", KeyPath: "/nonexistent/portmap-test-key"}
	if _, err := sshclient.AllocatePort(remote, 0, func(int) bool { return false }); err == nil || !strings.Contains(err.Error(), "无法检查") {
		t.Fatal("auth failure misreported as free port")
	}
}
