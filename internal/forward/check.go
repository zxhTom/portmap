package forward

import (
	"context"
	"fmt"
	"net"
)

// CheckTCP probes TCP reachability without sending application data.
func CheckTCP(ctx context.Context, addresses ...string) error {
	for _, address := range addresses {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return fmt.Errorf("TCP check %s: %w", address, err)
		}
		c.Close()
	}
	return nil
}
