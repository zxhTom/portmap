package relay

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"

	"github.com/zxhtom/portmap/internal/protocol"
)

type mappingState struct {
	mappingID  string
	targetHost string
	targetPort int
	localPort  int
	mux        *protocol.MuxConn
	listener   net.Listener
}

type Server struct {
	mu              sync.Mutex
	addr            string
	credentials     map[string]string
	listener        net.Listener
	closing         atomic.Bool
	mappings        map[string]*mappingState
	configuredPorts map[string]int
	streamID        atomic.Uint32
	handlers        map[uint32]net.Conn
	handlersL       sync.Mutex
}

func NewServer(addr string, credentials map[string]string) *Server {
	return &Server{
		addr:            addr,
		credentials:     credentials,
		mappings:        make(map[string]*mappingState),
		configuredPorts: make(map[string]int),
		handlers:        make(map[uint32]net.Conn),
	}
}

func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("relay listen %s: %w", s.addr, err)
	}
	s.listener = ln
	slog.Info("relay server started", "addr", s.addr)
	go s.acceptLoop()
	return nil
}

func (s *Server) Close() {
	s.closing.Store(true)
	if s.listener != nil {
		s.listener.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ms := range s.mappings {
		s.stopMappingLocked(ms)
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if !s.closing.Load() {
				slog.Error("relay accept error", "err", err)
			}
			return
		}
		slog.Info("relay client connected", "addr", conn.RemoteAddr())
		go s.handleClient(protocol.NewMuxConn(conn))
	}
}

func (s *Server) handleClient(mux *protocol.MuxConn) {
	mux.StartReadLoop(func(streamID uint32, data []byte) {
		s.handleStreamData(streamID, data)
	})

	authenticated := len(s.credentials) == 0
	for {
		select {
		case msg, ok := <-mux.CtrlChan():
			if !ok {
				return
			}
			switch msg.Type {
			case "auth":
				if len(s.credentials) > 0 {
					passwd, ok := s.credentials[msg.User]
					if ok && passwd == msg.Password {
						authenticated = true
						mux.SendCtrl(protocol.CtrlMsg{Type: "auth_ok"})
					} else {
						mux.SendCtrl(protocol.CtrlMsg{Type: "auth_error", Message: "invalid credentials"})
						mux.Conn().Close()
						return
					}
				} else if msg.Token != "" {
					_, ok := s.credentials[msg.Token]
					if len(s.credentials) == 0 || ok {
						authenticated = true
						mux.SendCtrl(protocol.CtrlMsg{Type: "auth_ok"})
					} else {
						mux.SendCtrl(protocol.CtrlMsg{Type: "auth_error", Message: "invalid token"})
						mux.Conn().Close()
						return
					}
				} else {
					authenticated = true
					mux.SendCtrl(protocol.CtrlMsg{Type: "auth_ok"})
				}
			case "register":
				if !authenticated {
					mux.SendCtrl(protocol.ErrorMsg(fmt.Errorf("not authenticated")))
					continue
				}
				port, err := s.registerMapping(msg.MappingID, msg.TargetHost, msg.TargetPort, mux)
				if err != nil {
					mux.SendCtrl(protocol.ErrorMsg(err))
					continue
				}
				mux.SendCtrl(protocol.CtrlMsg{Type: "registered", MappingID: msg.MappingID, LocalPort: port})
			case "deregister":
				s.deregisterMapping(msg.MappingID)
			case "closed":
				s.handleClientClose(mux)
				return
			}
		}
	}
}

func (s *Server) registerMapping(mappingID, targetHost string, targetPort int, mux *protocol.MuxConn) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.mappings[mappingID]; ok {
		if existing.listener != nil {
			existing.listener.Close()
			existing.listener = nil
		}
		existing.mux = mux
		existing.targetHost = targetHost
		existing.targetPort = targetPort
		if err := s.startMappingLocked(existing); err != nil {
			return 0, err
		}
		return existing.localPort, nil
	}

	localPort := 0
	if p, ok := s.configuredPorts[mappingID]; ok {
		localPort = p
	}

	ms := &mappingState{
		mappingID:  mappingID,
		targetHost: targetHost,
		targetPort: targetPort,
		localPort:  localPort,
		mux:        mux,
	}
	s.mappings[mappingID] = ms

	if err := s.startMappingLocked(ms); err != nil {
		delete(s.mappings, mappingID)
		return 0, err
	}
	return ms.localPort, nil
}

func (s *Server) startMappingLocked(ms *mappingState) error {
	if ms.listener != nil {
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", ms.localPort))
	if err != nil {
		return fmt.Errorf("listen :%d: %w", ms.localPort, err)
	}
	ms.listener = ln
	slog.Info("relay listener started", "mapping", ms.mappingID, "port", ms.localPort)
	go s.serveListener(ms)
	return nil
}

func (s *Server) SetPort(mappingID string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configuredPorts[mappingID] = port
	if ms, ok := s.mappings[mappingID]; ok && ms.listener != nil {
		ms.listener.Close()
		ms.listener = nil
		ms.localPort = port
		s.startMappingLocked(ms)
	}
}

func (s *Server) RegisteredMappings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.mappings))
	for id := range s.mappings {
		ids = append(ids, id)
	}
	return ids
}

func (s *Server) ActiveTunnels() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[string]bool, len(s.mappings))
	for id, ms := range s.mappings {
		result[id] = ms.listener != nil
	}
	return result
}

func (s *Server) MappingPort(mappingID string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.mappings[mappingID]
	if !ok {
		return 0, false
	}
	return ms.localPort, true
}

func (s *Server) DeregisterAndClose(mappingID string) {
	s.deregisterMapping(mappingID)
}

func (s *Server) deregisterMapping(mappingID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms, ok := s.mappings[mappingID]; ok {
		slog.Info("relay mapping deregistered", "mapping", mappingID)
		s.stopMappingLocked(ms)
		delete(s.mappings, mappingID)
	}
}

func (s *Server) stopMappingLocked(ms *mappingState) {
	if ms.listener != nil {
		ms.listener.Close()
		ms.listener = nil
		slog.Info("relay listener stopped", "mapping", ms.mappingID)
	}
}

func (s *Server) handleClientClose(mux *protocol.MuxConn) {
	s.mu.Lock()
	for id, ms := range s.mappings {
		if ms.mux == mux {
			slog.Info("relay client disconnected, cleaning up", "mapping", id)
			s.stopMappingLocked(ms)
			delete(s.mappings, id)
		}
	}
	s.mu.Unlock()
}

func (s *Server) serveListener(ms *mappingState) {
	for {
		conn, err := ms.listener.Accept()
		if err != nil {
			return
		}
		sid := s.streamID.Add(1)
		slog.Debug("relay stream open", "mapping", ms.mappingID, "stream_id", sid, "from", conn.RemoteAddr())

		s.handlersL.Lock()
		s.handlers[sid] = conn
		s.handlersL.Unlock()

		ms.mux.SendCtrl(protocol.CtrlMsg{
			Type:       "stream_open",
			StreamID:   sid,
			MappingID:  ms.mappingID,
			TargetHost: ms.targetHost,
			TargetPort: ms.targetPort,
		})

		go s.bridge(sid, conn, ms.mux)
	}
}

func (s *Server) bridge(streamID uint32, conn net.Conn, mux *protocol.MuxConn) {
	done := make(chan struct{}, 2)

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := mux.WriteFrame(streamID, buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				mux.SendCtrl(protocol.CtrlMsg{Type: "stream_close", StreamID: streamID})
				break
			}
		}
		done <- struct{}{}
	}()

	go func() {
		<-done
	}()

	<-done

	s.handlersL.Lock()
	if c, ok := s.handlers[streamID]; ok && c == conn {
		delete(s.handlers, streamID)
	}
	s.handlersL.Unlock()
	conn.Close()
	slog.Debug("relay stream closed", "stream_id", streamID)
}

func (s *Server) handleStreamData(streamID uint32, data []byte) {
	s.handlersL.Lock()
	conn, ok := s.handlers[streamID]
	s.handlersL.Unlock()
	if ok {
		conn.Write(data)
	}
}

func (s *Server) streamClose(streamID uint32) {
	s.handlersL.Lock()
	conn, ok := s.handlers[streamID]
	if ok {
		delete(s.handlers, streamID)
	}
	s.handlersL.Unlock()
	if ok {
		conn.Close()
	}
}
