package multipassclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/zxhtom/portmap/internal/config"
)

// Client only reads Multipass metadata; it never starts/stops/modifies instances.
type Client struct {
	binary string
	run    func(context.Context, ...string) ([]byte, error)
}

type Instance struct {
	Name    string   `json:"name"`
	State   string   `json:"state"`
	IPv4    []string `json:"ipv4"`
	Release string   `json:"release"`
}

func NewClient(binary string) *Client {
	if binary == "" {
		binary = os.Getenv("PORTMAP_MULTIPASS_BINARY")
	}
	if binary == "" {
		binary = "multipass"
		if _, err := exec.LookPath(binary); err != nil {
			if _, err := os.Stat("/snap/bin/multipass"); err == nil {
				binary = "/snap/bin/multipass"
			}
		}
	}
	c := &Client{binary: binary}
	c.run = func(ctx context.Context, args ...string) ([]byte, error) {
		data, err := exec.CommandContext(ctx, c.binary, args...).Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("Multipass command: %w", ctx.Err())
			}
			// Do not include CLI stderr, which may contain user configuration.
			return nil, fmt.Errorf("Multipass command failed (check installation and service-user access): %w", err)
		}
		return data, nil
	}
	return c
}

func (c *Client) List(ctx context.Context) ([]Instance, error) {
	data, err := c.run(ctx, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var result struct {
		List   []Instance `json:"list"`
		Errors []string   `json:"errors"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("invalid Multipass list JSON")
	}
	if len(result.Errors) > 0 {
		return nil, fmt.Errorf("Multipass reported errors while listing instances")
	}
	if result.List == nil {
		result.List = []Instance{}
	}
	return result.List, nil
}

func (c *Client) Resolve(ctx context.Context, name string) (string, error) {
	if !config.ValidInstanceName(name) {
		return "", fmt.Errorf("invalid Multipass instance name")
	}
	data, err := c.run(ctx, "info", "--format", "json", name)
	if err != nil {
		return "", err
	}
	var result struct {
		Info   map[string]Instance `json:"info"`
		Errors []string            `json:"errors"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("invalid Multipass info JSON")
	}
	if len(result.Errors) > 0 {
		return "", fmt.Errorf("Multipass reported an instance lookup error")
	}
	instance, ok := result.Info[name]
	if !ok {
		return "", fmt.Errorf("Multipass instance %s not found", name)
	}
	if !strings.EqualFold(instance.State, "Running") {
		return "", fmt.Errorf("Multipass instance %s is not running", name)
	}
	for _, addr := range instance.IPv4 {
		ip := net.ParseIP(addr)
		if ip != nil && ip.To4() != nil && ip.IsGlobalUnicast() && !ip.IsLoopback() {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("Multipass instance %s has no usable IPv4 address", name)
}
