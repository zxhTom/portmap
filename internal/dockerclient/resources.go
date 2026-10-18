package dockerclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

type ResourceManager struct {
	client     *Client
	instanceID string
	groupName  string
	mapName    string

	mu        sync.RWMutex
	networkID string
	targetID  string
	helperID  string
	targetIP  string
}

func NewResourceManager(client *Client, instanceID, groupName, mapName string) *ResourceManager {
	return &ResourceManager{
		client:     client,
		instanceID: instanceID,
		groupName:  groupName,
		mapName:    mapName,
	}
}

func (r *ResourceManager) NetworkID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.networkID
}

func (r *ResourceManager) HelperID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.helperID
}

func (r *ResourceManager) TargetIP() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.targetIP
}

func (r *ResourceManager) TargetID() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.targetID
}

func (r *ResourceManager) SetupResources(ctx context.Context, targetContainer, helperImage string) (setupErr error) {
	defer func() {
		if setupErr != nil {
			if rbErr := r.Rollback(); rbErr != nil {
				setupErr = fmt.Errorf("%w (rollback failed: %v)", setupErr, rbErr)
			}
		}
	}()

	cli := r.client.CommonClient()

	// 1. Inspect target container
	targetInspect, err := cli.ContainerInspect(ctx, targetContainer)
	if err != nil {
		return fmt.Errorf("inspect target container %s: %w", targetContainer, err)
	}
	if targetInspect.State == nil || !targetInspect.State.Running {
		return fmt.Errorf("target container %s is not running", targetContainer)
	}

	r.mu.Lock()
	r.targetID = targetInspect.ID
	targetID := r.targetID
	r.mu.Unlock()

	// 2. Ensure helper image is available
	if err := r.client.EnsureHelperImage(ctx, helperImage); err != nil {
		return fmt.Errorf("ensure helper image: %w", err)
	}

	// 3. Create dedicated network with ownership labels
	randomSuffix := make([]byte, 4)
	_, _ = rand.Read(randomSuffix)
	suffixStr := hex.EncodeToString(randomSuffix)
	netName := fmt.Sprintf("portmap-%s-%s-%s", r.instanceID, r.mapName, suffixStr)

	labels := map[string]string{
		LabelManaged:    "true",
		LabelInstanceID: r.instanceID,
		LabelGroup:      r.groupName,
		LabelMapping:    r.mapName,
	}

	netResp, err := cli.NetworkCreate(ctx, netName, network.CreateOptions{
		Driver: "bridge",
		Labels: labels,
	})
	if err != nil {
		return fmt.Errorf("create network %s: %w", netName, err)
	}

	r.mu.Lock()
	r.networkID = netResp.ID
	networkID := r.networkID
	r.mu.Unlock()

	// 4. Connect target container to network
	err = cli.NetworkConnect(ctx, networkID, targetID, nil)
	if err != nil {
		return fmt.Errorf("connect target container %s to network: %w", targetID, err)
	}

	// 5. Inspect target container to obtain IP address in this network
	targetInspectAfter, err := cli.ContainerInspect(ctx, targetID)
	if err != nil {
		return fmt.Errorf("inspect target after network connect: %w", err)
	}
	if targetInspectAfter.NetworkSettings == nil || targetInspectAfter.NetworkSettings.Networks == nil {
		return fmt.Errorf("target container has no network settings")
	}
	netSetting, ok := targetInspectAfter.NetworkSettings.Networks[netName]
	if !ok {
		for _, s := range targetInspectAfter.NetworkSettings.Networks {
			if s.NetworkID == networkID {
				netSetting = s
				ok = true
				break
			}
		}
	}
	if !ok || netSetting.IPAddress == "" {
		return fmt.Errorf("target container has no IP address in network %s", netName)
	}

	r.mu.Lock()
	r.targetIP = netSetting.IPAddress
	r.mu.Unlock()

	// 6. Create helper container
	helperName := fmt.Sprintf("portmap-helper-%s-%s-%s", r.instanceID, r.mapName, suffixStr)
	helperResp, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:  helperImage,
			Labels: labels,
			Cmd:    []string{"sleep", "infinity"},
		},
		&container.HostConfig{},
		&network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				netName: {
					NetworkID: networkID,
				},
			},
		},
		nil,
		helperName,
	)
	if err != nil {
		return fmt.Errorf("create helper container %s: %w", helperName, err)
	}

	r.mu.Lock()
	r.helperID = helperResp.ID
	helperID := r.helperID
	r.mu.Unlock()

	// 7. Start helper container
	if err := cli.ContainerStart(ctx, helperID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start helper container %s: %w", helperID, err)
	}

	return nil
}

func isIgnorableCleanupError(err error) bool {
	if err == nil {
		return true
	}
	if client.IsErrNotFound(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") || strings.Contains(msg, "is not connected") || strings.Contains(msg, "no such") {
		return true
	}
	return false
}

func (r *ResourceManager) Rollback() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli := r.client.CommonClient()
	var errs []error

	r.mu.Lock()
	helperID := r.helperID
	networkID := r.networkID
	targetID := r.targetID
	r.mu.Unlock()

	if helperID != "" {
		stopTimeout := 2
		_ = cli.ContainerStop(ctx, helperID, container.StopOptions{Timeout: &stopTimeout})
		if err := cli.ContainerRemove(ctx, helperID, container.RemoveOptions{Force: true}); err != nil && !isIgnorableCleanupError(err) {
			errs = append(errs, fmt.Errorf("remove helper container %s: %w", helperID, err))
		} else {
			r.mu.Lock()
			r.helperID = ""
			r.mu.Unlock()
		}
	}

	if networkID != "" {
		if targetID != "" {
			if err := cli.NetworkDisconnect(ctx, networkID, targetID, true); err != nil && !isIgnorableCleanupError(err) {
				errs = append(errs, fmt.Errorf("disconnect target %s from network %s: %w", targetID, networkID, err))
			}
		}
		if err := cli.NetworkRemove(ctx, networkID); err != nil && !isIgnorableCleanupError(err) {
			errs = append(errs, fmt.Errorf("remove network %s: %w", networkID, err))
		} else {
			r.mu.Lock()
			r.networkID = ""
			r.mu.Unlock()
		}
	}

	return errors.Join(errs...)
}
