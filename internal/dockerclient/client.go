package dockerclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/zxhtom/portmap/internal/config"
)

const (
	LabelManaged    = "portmap.managed"
	LabelInstanceID = "portmap.instance_id"
	LabelGroup      = "portmap.group"
	LabelMapping    = "portmap.mapping"
)

type PublishedPort struct {
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}

type ContainerSummary struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	State          string          `json:"state"`
	DeclaredPorts  []int           `json:"declared_ports"`
	PublishedPorts []PublishedPort `json:"published_ports"`
}

type Client struct {
	cli        client.CommonAPIClient
	instanceID string
}

func NewClient(cfg config.DockerConfig, instanceID string) (*Client, error) {
	host := cfg.Host
	if host == "" {
		host = config.DefaultDockerHost
	}

	opts := []client.Opt{
		client.WithHost(host),
		client.WithTimeout(10 * time.Second),
		client.WithAPIVersionNegotiation(),
	}

	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	return &Client{
		cli:        cli,
		instanceID: instanceID,
	}, nil
}

func NewWithClient(cli client.CommonAPIClient, instanceID string) *Client {
	return &Client{
		cli:        cli,
		instanceID: instanceID,
	}
}

func (c *Client) CommonClient() client.CommonAPIClient {
	return c.cli
}

type ControlledConn struct {
	Conn   net.Conn
	Reader *bufio.Reader
}

func (c *Client) ControlledAttach(ctx context.Context, execID string) (*ControlledConn, error) {
	type dialerProvider interface {
		Dialer() func(context.Context) (net.Conn, error)
		ClientVersion() string
	}

	dp, ok := c.cli.(dialerProvider)
	if !ok {
		return nil, fmt.Errorf("underlying docker client does not support raw dialer")
	}

	rawConn, err := dp.Dialer()(ctx)
	if err != nil {
		return nil, fmt.Errorf("dial docker daemon: %w", err)
	}

	// Register context cancellation watcher to close raw socket immediately if ctx canceled
	connClosed := make(chan struct{})
	closeOnce := syncOnceCloser(rawConn)
	defer func() {
		close(connClosed)
	}()

	go func() {
		select {
		case <-ctx.Done():
			closeOnce()
		case <-connClosed:
		}
	}()

	// Set deadline for HTTP Upgrade handshake
	handshakeDeadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(handshakeDeadline) {
		handshakeDeadline = d
	}
	_ = rawConn.SetDeadline(handshakeDeadline)

	bodyBytes, _ := json.Marshal(map[string]interface{}{
		"Detach": false,
		"Tty":    false,
	})

	apiVersion := dp.ClientVersion()
	path := fmt.Sprintf("/v%s/exec/%s/start", apiVersion, execID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(bodyBytes))
	if err != nil {
		closeOnce()
		return nil, fmt.Errorf("create upgrade request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	req.Host = "docker"

	if err := req.Write(rawConn); err != nil {
		closeOnce()
		return nil, fmt.Errorf("send upgrade request: %w", err)
	}

	br := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		closeOnce()
		return nil, fmt.Errorf("read upgrade response: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		closeOnce()
		return nil, fmt.Errorf("unexpected upgrade status: %s", resp.Status)
	}

	// Clear handshake deadline once upgrade succeeded
	_ = rawConn.SetDeadline(time.Time{})

	return &ControlledConn{
		Conn:   rawConn,
		Reader: br,
	}, nil
}

func syncOnceCloser(c io.Closer) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = c.Close()
		})
	}
}

func (c *Client) InstanceID() string {
	return c.instanceID
}

func (c *Client) ListContainers(ctx context.Context) ([]ContainerSummary, error) {
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("docker container list: %w", err)
	}

	var results []ContainerSummary
	for _, cnt := range containers {
		name := ""
		if len(cnt.Names) > 0 {
			name = strings.TrimPrefix(cnt.Names[0], "/")
		}

		var declared []int
		var published []PublishedPort
		seenDeclared := make(map[int]bool)

		for _, p := range cnt.Ports {
			if p.PrivatePort > 0 && !seenDeclared[int(p.PrivatePort)] {
				declared = append(declared, int(p.PrivatePort))
				seenDeclared[int(p.PrivatePort)] = true
			}
			if p.PublicPort > 0 {
				published = append(published, PublishedPort{
					HostIP:        p.IP,
					HostPort:      int(p.PublicPort),
					ContainerPort: int(p.PrivatePort),
					Protocol:      p.Type,
				})
			}
		}

		results = append(results, ContainerSummary{
			ID:             cnt.ID,
			Name:           name,
			State:          cnt.State,
			DeclaredPorts:  declared,
			PublishedPorts: published,
		})
	}

	return results, nil
}

func (c *Client) CleanupOwned(ctx context.Context) error {
	if c.instanceID == "" {
		return fmt.Errorf("refusing to cleanup with empty instanceID")
	}

	filter := filters.NewArgs(
		filters.Arg("label", fmt.Sprintf("%s=true", LabelManaged)),
		filters.Arg("label", fmt.Sprintf("%s=%s", LabelInstanceID, c.instanceID)),
	)

	var errs []error

	// 1. List owned helper containers and verify ownership
	containers, err := c.cli.ContainerList(ctx, container.ListOptions{
		All:     true,
		Filters: filter,
	})
	if err != nil {
		if !client.IsErrNotFound(err) {
			errs = append(errs, fmt.Errorf("list owned containers: %w", err))
		}
	} else {
		for _, cnt := range containers {
			if cnt.Labels[LabelManaged] != "true" || cnt.Labels[LabelInstanceID] != c.instanceID {
				continue
			}
			stopTimeout := 2
			if err := c.cli.ContainerStop(ctx, cnt.ID, container.StopOptions{Timeout: &stopTimeout}); err != nil {
				if !client.IsErrNotFound(err) {
					errs = append(errs, fmt.Errorf("stop helper container %s: %w", cnt.ID, err))
				}
			}
			if err := c.cli.ContainerRemove(ctx, cnt.ID, container.RemoveOptions{Force: true}); err != nil {
				if !client.IsErrNotFound(err) {
					errs = append(errs, fmt.Errorf("remove helper container %s: %w", cnt.ID, err))
				}
			}
		}
	}

	// 2. List owned networks, inspect to disconnect endpoints, then remove
	networks, err := c.cli.NetworkList(ctx, network.ListOptions{
		Filters: filter,
	})
	if err != nil {
		if !client.IsErrNotFound(err) {
			errs = append(errs, fmt.Errorf("list owned networks: %w", err))
		}
	} else {
		for _, netSummary := range networks {
			if netSummary.Labels[LabelManaged] != "true" || netSummary.Labels[LabelInstanceID] != c.instanceID {
				continue
			}

			netInspect, err := c.cli.NetworkInspect(ctx, netSummary.ID, network.InspectOptions{})
			if err == nil {
				for containerID := range netInspect.Containers {
					if err := c.cli.NetworkDisconnect(ctx, netSummary.ID, containerID, true); err != nil {
						if !client.IsErrNotFound(err) {
							errs = append(errs, fmt.Errorf("disconnect container %s from network %s: %w", containerID, netSummary.ID, err))
						}
					}
				}
			} else if !client.IsErrNotFound(err) {
				errs = append(errs, fmt.Errorf("inspect network %s: %w", netSummary.ID, err))
			}

			if err := c.cli.NetworkRemove(ctx, netSummary.ID); err != nil {
				if !client.IsErrNotFound(err) {
					errs = append(errs, fmt.Errorf("remove network %s: %w", netSummary.ID, err))
				}
			}
		}
	}

	return errors.Join(errs...)
}

type pullProgressMessage struct {
	Error       string `json:"error,omitempty"`
	ErrorDetail *struct {
		Message string `json:"message,omitempty"`
	} `json:"errorDetail,omitempty"`
}

func (c *Client) EnsureHelperImage(ctx context.Context, imgName string) error {
	images, err := c.cli.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", imgName)),
	})
	if err == nil && len(images) > 0 {
		return nil
	}

	rc, err := c.cli.ImagePull(ctx, imgName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull helper image %s: %w", imgName, err)
	}
	defer rc.Close()

	dec := json.NewDecoder(rc)
	for {
		var msg pullProgressMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read image pull stream: %w", err)
		}
		if msg.Error != "" {
			return fmt.Errorf("image pull failed: %s", msg.Error)
		}
		if msg.ErrorDetail != nil && msg.ErrorDetail.Message != "" {
			return fmt.Errorf("image pull failed: %s", msg.ErrorDetail.Message)
		}
	}

	return nil
}
