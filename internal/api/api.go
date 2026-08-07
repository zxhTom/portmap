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
	mux.HandleFunc("/api/mappings", s.handleMappings)
	mux.HandleFunc("/api/mappings/", s.handleMappingByPath)
	mux.HandleFunc("/api/groups/", s.handleGroupByPath)
	mux.HandleFunc("/api/logs", s.handleLogs)
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
	slog.Info("api server started", "addr", addr)
	return nil
}

func (s *Server) Close() {
	if s.ln != nil {
		s.ln.Close()
	}
}

func (s *Server) handleMappings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listMappings(w, r)
	case http.MethodPost:
		s.addMapping(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleMappingByPath(w http.ResponseWriter, r *http.Request) {
	parts := parsePath(r.URL.Path)
	if len(parts) < 2 {
		writeError(w, http.StatusBadRequest, "invalid path")
		return
	}

	const prefix = "/api/mappings/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	segments := strings.SplitN(rest, "/", 2)
	if len(segments) == 0 || segments[0] == "" {
		writeError(w, http.StatusBadRequest, "missing mapping name")
		return
	}

	name := segments[0]

	if len(segments) == 1 && r.Method == http.MethodPut {
		s.updateMapping(w, r, name)
		return
	}
	if len(segments) == 1 && r.Method == http.MethodDelete {
		s.deleteMapping(w, r, name)
		return
	}

	if len(segments) == 2 {
		action := segments[1]
		switch action {
		case "start":
			s.startMapping(w, r, name)
		case "stop":
			s.stopMapping(w, r, name)
		default:
			writeError(w, http.StatusNotFound, "unknown action")
		}
		return
	}

	writeError(w, http.StatusNotFound, "not found")
}

func (s *Server) handleGroupByPath(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/groups/"
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	segments := strings.SplitN(rest, "/", 2)

	if len(segments) == 0 || segments[0] == "" {
		writeError(w, http.StatusBadRequest, "missing group name")
		return
	}
	groupName := segments[0]
	if len(segments) == 2 {
		switch segments[1] {
		case "start":
			err := s.mgr.StartGroup(groupName)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "group " + groupName + " started"})
		case "stop":
			err := s.mgr.StopGroup(groupName)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "group " + groupName + " stopped"})
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
	LocalPort  int    `json:"local_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
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
		LocalPort:  req.LocalPort,
		TargetHost: req.TargetHost,
		TargetPort: req.TargetPort,
	}
	result, err := s.mgr.AddMapping(req.Group, mp, req.LocalPort == 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, result)
}

type updateMappingRequest struct {
	Group      string `json:"group"`
	LocalPort  int    `json:"local_port"`
	TargetHost string `json:"target_host"`
	TargetPort int    `json:"target_port"`
}

func (s *Server) updateMapping(w http.ResponseWriter, r *http.Request, name string) {
	var req updateMappingRequest
	if err := parseJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Group == "" {
		req.Group = "default"
	}
	mp := config.Mapping{
		Name:       name,
		LocalPort:  req.LocalPort,
		TargetHost: req.TargetHost,
		TargetPort: req.TargetPort,
	}
	result, err := s.mgr.UpdateMapping(req.Group, mp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, result)
}

func (s *Server) deleteMapping(w http.ResponseWriter, r *http.Request, name string) {
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "default"
	}
	if err := s.mgr.DeleteMapping(group, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"message": "deleted"})
}

func (s *Server) startMapping(w http.ResponseWriter, r *http.Request, name string) {
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "default"
	}
	if err := s.mgr.StartMapping(group, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"message": "started"})
}

func (s *Server) stopMapping(w http.ResponseWriter, r *http.Request, name string) {
	group := r.URL.Query().Get("group")
	if group == "" {
		group = "default"
	}
	if err := s.mgr.StopMapping(group, name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"message": "stopped"})
}

func (s *Server) listMappings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.mgr.ListMappings())
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "Logs are written to the configured log file or stderr.")
}

func parseJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func parsePath(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
