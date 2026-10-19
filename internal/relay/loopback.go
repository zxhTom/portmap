package relay

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// LoopbackOnly refuses activation when sshd's GatewayPorts=yes has overridden
// the requested loopback bind. Domain-only routes may never have public listeners.
func LoopbackOnly(port int) error {
	found := false
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			parts := strings.Split(fields[1], ":")
			if len(parts) != 2 {
				continue
			}
			p, _ := strconv.ParseInt(parts[1], 16, 32)
			if int(p) != port {
				continue
			}
			if parts[0] != "0100007F" && parts[0] != "00000000000000000000000001000000" {
				return fmt.Errorf("域名隧道端口 %d 正在公开监听，请将中转 sshd 的 GatewayPorts 设置为 clientspecified", port)
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("域名隧道端口 %d 尚未在 127.0.0.1 监听", port)
	}
	return nil
}
