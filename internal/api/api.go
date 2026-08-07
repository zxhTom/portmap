package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
)

type Server struct {
	mgr     *manager.Manager
	ln      net.Listener
	handler *http.ServeMux
}

func NewServer(mgr *manager.Manager) *Server {
	s := &Server{mgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remotes", s.handleRemotes)
	mux.HandleFunc("/api/remotes/", s.handleRemoteByPath)
	mux.HandleFunc("/api/mappings", s.handleMappings)
	mux.HandleFunc("/api/mappings/", s.handleMappingByPath)
	mux.HandleFunc("/api/groups/", s.handleGroupByPath)
	s.handler = mux
	return s
}

func (s *Server) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	go http.Serve(ln, s.handler)
	slog.Info("api started", "addr", addr)
	return nil
}

func (s *Server) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}

// --- remotes ---

func (s *Server) handleRemotes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.addRemote(w, r)
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func (s *Server) handleRemoteByPath(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/remotes/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, "invalid remote name")
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.mgr.DeleteRemote(name); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]string{"message": "deleted"})
		return
	}
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func (s *Server) addRemote(w http.ResponseWriter, r *http.Request) {
	var remote config.Remote
	if err := parseJSON(r, &remote); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.mgr.AddRemote(remote); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, remote)
}

// --- mappings ---

func (s *Server) handleMappings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.mgr.ListMappings())
	case http.MethodPost:
		s.addMapping(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleMappingByPath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/mappings/")
	segments := strings.SplitN(rest, "/", 2)
	name := segments[0]
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "default"
	}

	if len(segments) == 1 && r.Method == http.MethodDelete {
		if err := s.mgr.DeleteMapping(group, name); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, map[string]string{"message": "deleted"})
		return
	}
	if len(segments) == 2 {
		switch segments[1] {
		case "start":
			if err := s.mgr.StartMapping(group, name); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "started"})
		case "stop":
			if err := s.mgr.StopMapping(group, name); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "stopped"})
		default:
			writeError(w, http.StatusNotFound, "unknown action")
		}
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

type addMappingRequest struct {
	Group      string `json:"group"`
	Name       string `json:"name"`
	Remote     string `json:"remote"`
	ListenPort int    `json:"listen_port"`
	LocalHost  string `json:"local_host"`
	LocalPort  int    `json:"local_port"`
}

func (s *Server) addMapping(w http.ResponseWriter, r *http.Request) {
	var req addMappingRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Group == "" {
		req.Group = "default"
	}
	mp := config.Mapping{
		Name:       req.Name,
		Remote:     req.Remote,
		ListenPort: req.ListenPort,
		LocalHost:  req.LocalHost,
		LocalPort:  req.LocalPort,
	}
	if err := s.mgr.AddMapping(req.Group, mp); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, mp)
}

// --- groups ---

func (s *Server) handleGroupByPath(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	segments := strings.SplitN(rest, "/", 2)
	groupName := segments[0]
	if len(segments) == 2 {
		switch segments[1] {
		case "start":
			err := s.mgr.StartGroup(groupName)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "started"})
		case "stop":
			err := s.mgr.StopGroup(groupName)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "stopped"})
		default:
			writeError(w, http.StatusNotFound, "unknown action")
		}
		return
	}
	writeError(w, http.StatusNotFound, "not found")
}

// --- helpers ---

func parseJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":%q}`+"\n", msg)
}
