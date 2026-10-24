package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/zxhtom/portmap/internal/api"
	"github.com/zxhtom/portmap/internal/config"
	"github.com/zxhtom/portmap/internal/manager"
	"github.com/zxhtom/portmap/internal/multipassclient"
	"github.com/zxhtom/portmap/internal/webui"
)

var (
	domainPrefixes []string
	upstreamScheme string

	webAddr, webCert, webKey string
	dataDir                  string
	apiAddr                  string
	logFile                  string
	group                    string
	mappingType              string
	description              string
	listenPort               int
	localHost                string
	localPort                int
	containerName            string
	containerPort            int
	bindHost                 string
	instanceName             string
	instancePort             int
	dockerHost               string
	dockerHelperImage        string
	remoteName               string
	remoteHost               string
	remotePort               int
	remoteUser               string
	authMethod               string
	authPass                 string
	authKey                  string
	authPhrase               string
)

func dataDirPath() string {
	if dataDir != "" {
		return dataDir
	}
	if d := os.Getenv("PORTMAP_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "portmap")
}

func initSlog() {
	var w io.Writer = os.Stderr
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot open log file %s: %v\n", logFile, err)
			os.Exit(1)
		}
		w = io.MultiWriter(os.Stderr, f)
	}
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h))
}

func apiURL(path string) string {
	return "http://" + apiAddr + path
}

func httpClient() *http.Client { return &http.Client{} }

var rootCmd = &cobra.Command{Use: "portmap", Short: "SSH reverse tunnel manager"}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the portmap daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		initSlog()
		dir := dataDirPath()
		mgr := manager.New(dir)

		var hostOverride, imageOverride *string
		if cmd.Flags().Changed("docker-host") {
			hostOverride = &dockerHost
		}
		if cmd.Flags().Changed("docker-helper-image") {
			imageOverride = &dockerHelperImage
		}
		mgr.SetDockerOverrides(hostOverride, imageOverride)

		if err := mgr.Init(); err != nil {
			return fmt.Errorf("init: %w", err)
		}
		// Stop reconciliation before closing backends and releasing the instance lock.
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); mgr.Run(ctx) }()
		defer func() { cancel(); <-done; mgr.Shutdown() }()
		srv := api.NewServer(mgr)
		if err := srv.Listen(apiAddr); err != nil {
			return fmt.Errorf("api: %w", err)
		}
		defer srv.Close()
		if webAddr != "" {
			web, err := webui.New(mgr, srv.Handler())
			if err != nil {
				slog.Error("web console unavailable", "err", err)
			} else if err := web.Listen(webAddr, webCert, webKey); err != nil {
				slog.Error("web console unavailable", "err", err)
			} else {
				defer web.Close()
				slog.Info("web console started", "addr", webAddr)
			}
		}
		slog.Info("portmap started", "dir", dir, "api", apiAddr)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		<-sigCh
		slog.Info("shutting down...")
		srv.Close()
		return nil
	},
}

// --- remote commands ---

var remoteAddCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a remote server (the VPS)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req := map[string]interface{}{
			"name": args[0],
			"host": remoteHost,
			"port": remotePort,
			"user": remoteUser,
			"auth": map[string]interface{}{
				"method":     authMethod,
				"password":   authPass,
				"key_path":   authKey,
				"passphrase": authPhrase,
			},
		}
		return apiPost("/api/remotes", req)
	},
}

var remoteDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a remote",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiDelete("/api/remotes/" + args[0])
	},
}

var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Manage remote servers (VPS)",
}

// --- mapping commands ---

var addCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a port mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if mappingType == "" {
			mappingType = "ssh"
		}
		if mappingType != "ssh" && mappingType != "docker" && mappingType != "multipass" {
			return fmt.Errorf("invalid type: %q (must be 'ssh', 'docker' or 'multipass')", mappingType)
		}
		if listenPort < 0 || listenPort > 65535 {
			return fmt.Errorf("--listen must be 1–65535; omit it for a random relay port")
		}

		req := map[string]interface{}{
			"group":           group,
			"name":            args[0],
			"type":            mappingType,
			"listen_port":     listenPort,
			"description":     description,
			"expires_at":      addExpiresAt,
			"domain_prefixes": domainPrefixes, "upstream_scheme": upstreamScheme,
			"domain_zone": cmd.Flag("domain-zone").Value.String(),
		}

		if mappingType == "ssh" {
			if remoteName == "" {
				return fmt.Errorf("--remote is required for ssh mapping")
			}
			if localPort <= 0 || localPort > 65535 {
				return fmt.Errorf("--local-port must be between 1 and 65535 for ssh mapping")
			}
			req["remote"] = remoteName
			req["local_host"] = localHost
			req["local_port"] = localPort
		} else if mappingType == "multipass" {
			if !config.ValidInstanceName(instanceName) {
				return fmt.Errorf("--instance must be a Multipass instance name")
			}
			if instancePort < 1 || instancePort > 65535 {
				return fmt.Errorf("--instance-port must be between 1 and 65535")
			}
			req["instance"] = instanceName
			req["instance_port"] = instancePort
			req["bind_host"] = bindHost
			req["remote"] = remoteName
		} else { // docker
			if containerName == "" {
				return fmt.Errorf("--container is required for docker mapping")
			}
			if containerPort <= 0 || containerPort > 65535 {
				return fmt.Errorf("--container-port must be between 1 and 65535 for docker mapping")
			}
			req["container"] = containerName
			req["container_port"] = containerPort
			if bindHost == "" {
				bindHost = "127.0.0.1"
			}
			req["bind_host"] = bindHost
		}

		return apiPost("/api/mappings", req)
	},
}

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a port mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiDelete(fmt.Sprintf("/api/mappings/%s?group=%s", args[0], group))
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all mappings",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := httpClient().Get(apiURL("/api/mappings"))
		if err != nil {
			return fmt.Errorf("request: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("server error: %s", body)
		}
		var mappings []manager.MappingInfo
		if err := json.NewDecoder(resp.Body).Decode(&mappings); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "GROUP\tNAME\tTYPE\tREMOTE/BIND\tSTATUS\tHEALTH/FAILURES\tLISTEN\tTARGET\tSTARTED\tEXPIRES\tREASON\tDOMAINS\tDESCRIPTION")
		for _, m := range mappings {
			mpType := m.Type
			if mpType == "" {
				mpType = "ssh"
			}
			remoteOrBind := m.Remote
			target := fmt.Sprintf("%s:%d", m.LocalHost, m.LocalPort)
			if mpType == "docker" {
				remoteOrBind = m.BindHost
				target = fmt.Sprintf("%s:%d", m.Container, m.ContainerPort)
			}
			if mpType == "multipass" {
				if m.Remote == "" {
					remoteOrBind = m.BindHost
				}
				target = fmt.Sprintf("%s:%d", m.Instance, m.InstancePort)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s/%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				m.Group, m.Name, mpType, remoteOrBind, m.Status, m.Health, m.ConsecutiveFailures, m.ListenPort, target, m.StartedAt, displayExpiry(m.ExpiresAt), displayInvalidReason(m.InvalidReason), strings.Join(m.Domains, ","), strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(m.Description))
		}
		return w.Flush()
	},
}

// --- docker commands ---

var dockerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List discoverable Docker containers and ports",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := httpClient().Get(apiURL("/api/docker/containers"))
		if err != nil {
			return fmt.Errorf("request: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("docker error (%d): %s", resp.StatusCode, body)
		}
		var containers []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			State          string `json:"state"`
			DeclaredPorts  []int  `json:"declared_ports"`
			PublishedPorts []struct {
				HostIP        string `json:"host_ip"`
				HostPort      int    `json:"host_port"`
				ContainerPort int    `json:"container_port"`
				Protocol      string `json:"protocol"`
			} `json:"published_ports"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "CONTAINER ID\tNAME\tSTATE\tDECLARED PORTS\tPUBLISHED PORTS")
		for _, c := range containers {
			var dec []string
			for _, p := range c.DeclaredPorts {
				dec = append(dec, fmt.Sprintf("%d", p))
			}
			var pub []string
			for _, p := range c.PublishedPorts {
				pub = append(pub, fmt.Sprintf("%s:%d->%d/%s", p.HostIP, p.HostPort, p.ContainerPort, p.Protocol))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				c.ID, c.Name, c.State, strings.Join(dec, ","), strings.Join(pub, ","))
		}
		return w.Flush()
	},
}

var dockerCmd = &cobra.Command{
	Use:   "docker",
	Short: "Docker management",
}

var startCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiPost(fmt.Sprintf("/api/mappings/%s/start?group=%s", args[0], group), nil)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiPost(fmt.Sprintf("/api/mappings/%s/stop?group=%s", args[0], group), nil)
	},
}

var groupStartCmd = &cobra.Command{
	Use:   "start <group>",
	Short: "Start all mappings in a group",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiPost(fmt.Sprintf("/api/groups/%s/start", args[0]), nil)
	},
}

var groupStopCmd = &cobra.Command{
	Use:   "stop <group>",
	Short: "Stop all mappings in a group",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return apiPost(fmt.Sprintf("/api/groups/%s/stop", args[0]), nil)
	},
}

var groupCmd = &cobra.Command{Use: "group", Short: "Group management"}

// --- http helpers ---

func apiPost(url string, body interface{}) error {
	var r io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		r = bytes.NewReader(data)
	}
	resp, err := httpClient().Post(apiURL(url), "application/json", r)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", respBody)
	}
	if body != nil {
		io.Copy(os.Stdout, resp.Body)
		fmt.Println()
	}
	return nil
}

func apiDelete(url string) error {
	req, _ := http.NewRequest(http.MethodDelete, apiURL(url), nil)
	resp, err := httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", respBody)
	}
	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
	return nil
}

var multipassCmd = &cobra.Command{Use: "multipass", Short: "Discover Multipass instances"}
var multipassListCmd = &cobra.Command{
	Use: "list", Short: "List Multipass instances, states and IPv4 addresses",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := httpClient().Get(apiURL("/api/multipass/instances"))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("Multipass error (%d): %s", resp.StatusCode, body)
		}
		var instances []multipassclient.Instance
		if err := json.NewDecoder(resp.Body).Decode(&instances); err != nil {
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tSTATE\tIPV4\tRELEASE")
		for _, vm := range instances {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", vm.Name, vm.State, strings.Join(vm.IPv4, ","), vm.Release)
		}
		return w.Flush()
	},
}

var describeCmd = &cobra.Command{Use: "describe <name> <description>", Short: "Set or clear a mapping description without restarting it", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	data, err := json.Marshal(map[string]string{"description": args[1]})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, apiURL("/api/mappings/"+url.PathEscape(args[0])+"/description?group="+url.QueryEscape(group)), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s", body)
	}
	fmt.Fprintln(cmd.OutOrStdout(), string(body))
	return nil
}}

var versionCmd = &cobra.Command{Use: "version", Short: "Show version and upgrade capabilities", RunE: func(cmd *cobra.Command, args []string) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]interface{}{"version": buildVersion, "config_version": 1, "domain_routing": true})
}}
var buildVersion = "dev"

func init() {
	rootCmd.PersistentFlags().StringVar(&dataDir, "data-dir", "", "Data directory (default ~/.config/portmap)")
	rootCmd.PersistentFlags().StringVar(&apiAddr, "api-addr", "127.0.0.1:9876", "API address")

	serveCmd.Flags().StringVar(&webAddr, "web-addr", "127.0.0.1:9877", "Web console address (empty disables it)")
	serveCmd.Flags().StringVar(&webCert, "web-cert", "", "TLS certificate for public web access")
	serveCmd.Flags().StringVar(&webKey, "web-key", "", "TLS private key for public web access")
	serveCmd.Flags().StringVar(&logFile, "log-file", "", "Log file path")
	serveCmd.Flags().StringVar(&dockerHost, "docker-host", "", "Docker daemon host (e.g. unix:///var/run/docker.sock)")
	serveCmd.Flags().StringVar(&dockerHelperImage, "docker-helper-image", "", "Docker helper image for port-forwarding")

	// remote add flags
	remoteAddCmd.Flags().StringVar(&remoteHost, "host", "", "VPS hostname or IP (required)")
	remoteAddCmd.Flags().IntVar(&remotePort, "port", 22, "SSH port")
	remoteAddCmd.Flags().StringVarP(&remoteUser, "user", "u", "root", "SSH user")
	remoteAddCmd.Flags().StringVar(&authMethod, "auth-method", "key", "Auth method: password or key")
	remoteAddCmd.Flags().StringVar(&authPass, "password", "", "SSH password")
	remoteAddCmd.Flags().StringVar(&authKey, "key", "~/.ssh/id_rsa", "SSH private key path")
	remoteAddCmd.Flags().StringVar(&authPhrase, "passphrase", "", "Key passphrase")
	remoteAddCmd.MarkFlagRequired("host")

	addCmd.Flags().StringVar(&addExpiresAt, "expires-at", "", "Expiry time in RFC3339 (default: no expiry)")
	addCmd.Flags().String("domain-zone", "", "Suffix bound to the selected VPS (default: its primary suffix)")
	addCmd.Flags().StringSliceVar(&domainPrefixes, "domain", nil, "Domain prefix(es), e.g. portmap or app,api; HTTP services only")
	addCmd.Flags().StringVar(&upstreamScheme, "upstream-scheme", "http", "Domain upstream protocol: http or https")
	// add mapping flags
	addCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	addCmd.Flags().StringVarP(&mappingType, "type", "t", "ssh", "Mapping type: ssh, docker or multipass")
	addCmd.Flags().StringVarP(&remoteName, "remote", "r", "", "Remote name (VPS, required for ssh)")
	addCmd.Flags().IntVarP(&listenPort, "listen", "l", 0, "Listen port; omit for a random available 5-digit relay port")
	addCmd.Flags().StringVar(&localHost, "local-host", "127.0.0.1", "Local target host (for ssh)")
	addCmd.Flags().IntVarP(&localPort, "local-port", "p", 0, "Local target port (for ssh)")
	addCmd.Flags().StringVar(&containerName, "container", "", "Target container name or ID (for docker)")
	addCmd.Flags().IntVar(&containerPort, "container-port", 0, "Target container port (for docker)")
	addCmd.Flags().StringVar(&bindHost, "bind-host", "127.0.0.1", "Host bind IP address (for docker/multipass)")
	addCmd.Flags().StringVar(&instanceName, "instance", "", "Multipass instance name")
	addCmd.Flags().IntVar(&instancePort, "instance-port", 0, "Target TCP port inside the Multipass instance")
	addCmd.Flags().StringVar(&description, "description", "", "What this mapping is used for")
	describeCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")

	deleteCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	startCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	stopCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")

	multipassCmd.AddCommand(multipassListCmd)
	dockerCmd.AddCommand(dockerListCmd)
	groupCmd.AddCommand(groupStartCmd, groupStopCmd)
	remoteCmd.AddCommand(remoteAddCmd, remoteDeleteCmd)

	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(serveCmd, remoteCmd, addCmd, deleteCmd, listCmd, startCmd, stopCmd, groupCmd, dockerCmd, multipassCmd, describeCmd, renewCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
