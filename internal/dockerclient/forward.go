package dockerclient

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/forward"
)

type Forward struct {
	client     *Client
	resMgr     *ResourceManager
	mapping    config.Mapping
	groupName  string
	instanceID string
	helperImg  string

	mu        sync.Mutex
	status    forward.Status
	ln        net.Listener
	closed    bool
	closing   chan struct{}
	connsMu   sync.Mutex
	conns     map[net.Conn]struct{}
	sessionWg sync.WaitGroup
	monitorWg sync.WaitGroup
	serveWg   sync.WaitGroup
}

func NewForward(client *Client, mapping config.Mapping, groupName, instanceID, helperImage string) *Forward {
	if helperImage == "" {
		helperImage = config.DefaultDockerHelperImage
	}
	mapping.SetDefaults()
	resMgr := NewResourceManager(client, instanceID, groupName, mapping.Name)
	return &Forward{
		client:     client,
		resMgr:     resMgr,
		mapping:    mapping,
		groupName:  groupName,
		instanceID: instanceID,
		helperImg:  helperImage,
		status:     forward.Status{State: forward.StateStopped},
		closing:    make(chan struct{}),
		conns:      make(map[net.Conn]struct{}),
	}
}

func (f *Forward) Status() forward.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *Forward) Start() error {
	f.mu.Lock()
	if f.closed {
		f.closing = make(chan struct{})
		f.closed = false
	}
	f.status = forward.Status{State: forward.StateStarting}
	f.mu.Unlock()

	bindAddr := net.JoinHostPort(f.mapping.BindHost, strconv.Itoa(f.mapping.ListenPort))
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		f.setStatus(forward.StateFailed, fmt.Sprintf("bind host listener %s: %v", bindAddr, err))
		return fmt.Errorf("bind host %s: %w", bindAddr, err)
	}

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelSetup()

	if err := f.resMgr.SetupResources(setupCtx, f.mapping.Container, f.helperImg); err != nil {
		_ = ln.Close()
		f.setStatus(forward.StateFailed, fmt.Sprintf("setup resources: %v", err))
		return fmt.Errorf("setup docker resources: %w", err)
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = ln.Close()
		_ = f.resMgr.Rollback()
		return fmt.Errorf("forward closed while starting")
	}
	f.ln = ln
	f.status = forward.Status{State: forward.StateRunning}
	f.mu.Unlock()

	f.serveWg.Add(1)
	go f.serve(ln)

	f.monitorWg.Add(1)
	go f.monitorLifecycle()

	return nil
}

func (f *Forward) setStatus(st forward.State, errMsg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = forward.Status{State: st, Error: errMsg}
}

func (f *Forward) serve(ln net.Listener) {
	defer f.serveWg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-f.closing:
				return
			default:
				f.mu.Lock()
				// Do not overwrite failure reason if monitor already set failed
				if f.status.State != forward.StateFailed {
					f.status = forward.Status{State: forward.StateFailed, Error: fmt.Sprintf("listener accept error: %v", err)}
				}
				f.mu.Unlock()
				return
			}
		}

		f.registerConn(conn)
		f.sessionWg.Add(1)
		go func(c net.Conn) {
			defer f.sessionWg.Done()
			f.handleSession(c)
		}(conn)
	}
}

func (f *Forward) registerConn(c net.Conn) {
	f.connsMu.Lock()
	defer f.connsMu.Unlock()
	f.conns[c] = struct{}{}
}

func (f *Forward) unregisterConn(c net.Conn) {
	f.connsMu.Lock()
	defer f.connsMu.Unlock()
	delete(f.conns, c)
}

type closeWriter interface {
	CloseWrite() error
}

func (f *Forward) handleSession(clientConn net.Conn) {
	defer func() {
		_ = clientConn.Close()
		f.unregisterConn(clientConn)
	}()

	cli := f.client.CommonClient()
	targetIP := f.resMgr.TargetIP()
	helperID := f.resMgr.HelperID()

	if targetIP == "" || helperID == "" {
		return
	}

	execCmd := []string{
		"socat",
		"STDIO",
		fmt.Sprintf("TCP-CONNECT:%s:%d", targetIP, f.mapping.ContainerPort),
	}

	createCtx, cancelCreate := context.WithTimeout(context.Background(), 10*time.Second)
	execResp, err := cli.ContainerExecCreate(createCtx, helperID, container.ExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          execCmd,
		Tty:          false,
	})
	cancelCreate()
	if err != nil {
		slog.Error("container exec create failed", "err", err)
		return
	}

	// Controlled upgrade context that aborts if f.closing is signaled
	attachCtx, cancelAttach := context.WithCancel(context.Background())
	defer cancelAttach()

	go func() {
		select {
		case <-f.closing:
			cancelAttach()
		case <-attachCtx.Done():
		}
	}()

	ctrlConn, err := f.client.ControlledAttach(attachCtx, execResp.ID)
	if err != nil {
		slog.Error("controlled attach failed", "err", err)
		return
	}
	rawConn := ctrlConn.Conn
	streamReader := ctrlConn.Reader

	defer rawConn.Close()
	f.registerConn(rawConn)
	defer f.unregisterConn(rawConn)

	clientDone := make(chan struct{})
	serverDone := make(chan struct{})

	// Direction 1: Client -> Helper Exec Stdin
	go func() {
		defer func() {
			if cw, ok := rawConn.(closeWriter); ok {
				_ = cw.CloseWrite()
			}
			close(clientDone)
		}()
		_, _ = io.Copy(rawConn, clientConn)
	}()

	// Direction 2: Helper Exec Stream -> Client (demux stdout)
	go func() {
		defer func() {
			if cw, ok := clientConn.(closeWriter); ok {
				_ = cw.CloseWrite()
			}
			close(serverDone)
		}()
		_ = DemuxStdoutStream(streamReader, clientConn)
	}()

	select {
	case <-serverDone:
	case <-f.closing:
	}
}

type typesHijackedResponse struct {
	Conn   net.Conn
	Reader io.Reader
}

func DemuxStdoutStream(r io.Reader, out io.Writer) error {
	header := make([]byte, 8)
	for {
		_, err := io.ReadFull(r, header)
		if err != nil {
			return err
		}
		streamType := header[0]
		frameSize := binary.BigEndian.Uint32(header[4:8])

		if streamType == 1 {
			if _, err := io.CopyN(out, r, int64(frameSize)); err != nil {
				return err
			}
		} else {
			if _, err := io.CopyN(io.Discard, r, int64(frameSize)); err != nil {
				return err
			}
		}
	}
}

func (f *Forward) monitorLifecycle() {
	defer f.monitorWg.Done()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	cli := f.client.CommonClient()
	targetID := f.resMgr.TargetID()
	helperID := f.resMgr.HelperID()

	for {
		select {
		case <-f.closing:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

			targetInspect, err := cli.ContainerInspect(ctx, targetID)
			if err != nil || targetInspect.State == nil || !targetInspect.State.Running {
				cancel()
				f.handleFailure("target container stopped or unavailable")
				return
			}

			helperInspect, err := cli.ContainerInspect(ctx, helperID)
			cancel()
			if err != nil || helperInspect.State == nil || !helperInspect.State.Running {
				f.handleFailure("helper container stopped or abnormal exit")
				return
			}
		}
	}
}

func (f *Forward) handleFailure(reason string) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.status = forward.Status{State: forward.StateFailed, Error: reason}
	ln := f.ln
	f.ln = nil
	f.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}

	f.closeConns()
	_ = f.resMgr.Rollback()
}

func (f *Forward) closeConns() {
	f.connsMu.Lock()
	defer f.connsMu.Unlock()
	for c := range f.conns {
		_ = c.Close()
		delete(f.conns, c)
	}
}

func (f *Forward) Close() error {
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.closing)

		ln := f.ln
		f.ln = nil
		f.status = forward.Status{State: forward.StateStopped}
		f.mu.Unlock()

		if ln != nil {
			_ = ln.Close()
		}

		f.closeConns()
		f.serveWg.Wait()
		f.sessionWg.Wait()
		f.monitorWg.Wait()
	} else {
		f.mu.Unlock()
	}

	return f.resMgr.Rollback()
}
