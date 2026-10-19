package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method not allowed")
			return
		}
		dir, err := filepath.Abs(mgr.DataDir())
		if err != nil {
			writeError(w, 500, "cannot resolve data directory")
			return
		}
		writeJSON(w, map[string]interface{}{"pid": os.Getpid(), "data_dir": dir, "config_version": 1})
	})
	mux.HandleFunc("/api/remotes", s.handleRemotes)
	mux.HandleFunc("/api/settings/domain-zone", s.handleDomainZone)
	mux.HandleFunc("/api/remotes/", s.handleRemoteByPath)
	mux.HandleFunc("/api/mappings", s.handleMappings)
	mux.HandleFunc("/api/mappings/", s.handleMappingByPath)
	mux.HandleFunc("/api/groups/", s.handleGroupByPath)
	mux.HandleFunc("/api/docker/containers", s.handleDockerContainers)
	mux.HandleFunc("/api/multipass/instances", s.handleMultipassInstances)
	s.handler = mux
	return s
}

func (s *Server) handleDomainZone(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, map[string]string{"domain_zone": s.mgr.DomainZone()})
	case http.MethodPut:
		var body struct {
			DomainZone string `json:"domain_zone"`
		}
		if err := parseJSON(r, &body); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := s.mgr.SetDomainZone(body.DomainZone); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]string{"domain_zone": s.mgr.DomainZone()})
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (s *Server) Handler() http.Handler {
	return s.handler
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

	if len(segments) == 2 && segments[1] == "domains" {
		if r.Method != http.MethodPut {
			writeError(w, 405, "method not allowed")
			return
		}
		var req struct {
			Prefixes *[]string `json:"domain_prefixes"`
			Scheme   string    `json:"upstream_scheme"`
		}
		if err := parseJSON(r, &req); err != nil || req.Prefixes == nil {
			writeError(w, 400, "domain_prefixes is required; an empty list removes domain routing")
			return
		}
		if err := s.mgr.SetDomains(group, name, *req.Prefixes, req.Scheme); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]string{"message": "domain bindings updated"})
		return
	}
	if len(segments) == 2 && segments[1] == "expiry" {
		if r.Method != http.MethodPut {
			writeError(w, 405, "method not allowed")
			return
		}
		var req struct {
			ExpiresAt *string `json:"expires_at"`
		}
		if err := parseJSON(r, &req); err != nil || req.ExpiresAt == nil {
			writeError(w, 400, "expires_at is required (empty string removes the limit)")
			return
		}
		if err := s.mgr.SetExpiry(group, name, *req.ExpiresAt); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]string{"message": "validity updated"})
		return
	}
	if len(segments) == 2 && segments[1] == "description" {
		if r.Method != http.MethodPut {
			writeError(w, 405, "method not allowed")
			return
		}
		var req struct {
			Description *string `json:"description"`
		}
		if err := parseJSON(r, &req); err != nil || req.Description == nil {
			writeError(w, 400, "description is required (empty string clears it)")
			return
		}
		if err := s.mgr.DescribeMapping(group, name, *req.Description); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]string{"message": "description updated"})
		return
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
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			if err := s.mgr.StartMapping(group, name); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "started"})
		case "stop":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
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
	DomainPrefixes []string `json:"domain_prefixes"`
	UpstreamScheme string   `json:"upstream_scheme"`
	ExpiresAt      string   `json:"expires_at"`
	Description    string   `json:"description"`
	Group          string   `json:"group"`
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Remote         string   `json:"remote"`
	ListenPort     int      `json:"listen_port"`
	LocalHost      string   `json:"local_host"`
	LocalPort      int      `json:"local_port"`
	Container      string   `json:"container"`
	ContainerPort  int      `json:"container_port"`
	BindHost       string   `json:"bind_host"`
	Instance       string   `json:"instance"`
	InstancePort   int      `json:"instance_port"`
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
		DomainPrefixes: req.DomainPrefixes, UpstreamScheme: req.UpstreamScheme,
		ExpiresAt:     req.ExpiresAt,
		Name:          req.Name,
		Description:   req.Description,
		Type:          req.Type,
		Remote:        req.Remote,
		ListenPort:    req.ListenPort,
		LocalHost:     req.LocalHost,
		LocalPort:     req.LocalPort,
		Container:     req.Container,
		ContainerPort: req.ContainerPort,
		BindHost:      req.BindHost,
		Instance:      req.Instance,
		InstancePort:  req.InstancePort,
	}
	created, err := s.mgr.CreateMapping(req.Group, mp)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	mp = created
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
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			err := s.mgr.StartGroup(groupName)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]string{"message": "started"})
		case "stop":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
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

// --- docker ---

func (s *Server) handleDockerContainers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	containers, err := s.mgr.ListDockerContainers()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, containers)
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

func (s *Server) handleMultipassInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	instances, err := s.mgr.ListMultipassInstances()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, instances)
}
