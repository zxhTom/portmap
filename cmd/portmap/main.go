package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/zxhtom/portmap/internal/api"
	"github.com/zxhtom/portmap/internal/manager"
)

var (
	dataDir    string
	apiAddr    string
	logFile    string
	group      string
	listenPort int
	localHost  string
	localPort  int
	remoteName string
	remoteHost string
	remotePort int
	remoteUser string
	authMethod string
	authPass   string
	authKey    string
	authPhrase string
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
		if err := mgr.Init(); err != nil {
			return fmt.Errorf("init: %w", err)
		}
		srv := api.NewServer(mgr)
		if err := srv.Listen(apiAddr); err != nil {
			return fmt.Errorf("api: %w", err)
		}
		slog.Info("portmap started", "dir", dir, "api", apiAddr)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		slog.Info("shutting down...")
		srv.Close()
		mgr.Shutdown()
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
		req := map[string]interface{}{
			"group":       group,
			"name":        args[0],
			"remote":      remoteName,
			"listen_port": listenPort,
			"local_host":  localHost,
			"local_port":  localPort,
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
		fmt.Fprintln(w, "GROUP\tNAME\tREMOTE\tSTATUS\tLISTEN\tTARGET\tSTARTED")
		for _, m := range mappings {
			target := fmt.Sprintf("%s:%d", m.LocalHost, m.LocalPort)
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				m.Group, m.Name, m.Remote, m.Status, m.ListenPort, target, m.StartedAt)
		}
		return w.Flush()
	},
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

func main() {
	rootCmd.PersistentFlags().StringVar(&dataDir, "data-dir", "", "Data directory (default ~/.config/portmap)")
	rootCmd.PersistentFlags().StringVar(&apiAddr, "api-addr", "127.0.0.1:9876", "API address")

	serveCmd.Flags().StringVar(&logFile, "log-file", "", "Log file path")

	// remote add flags
	remoteAddCmd.Flags().StringVar(&remoteHost, "host", "", "VPS hostname or IP (required)")
	remoteAddCmd.Flags().IntVar(&remotePort, "port", 22, "SSH port")
	remoteAddCmd.Flags().StringVarP(&remoteUser, "user", "u", "root", "SSH user")
	remoteAddCmd.Flags().StringVar(&authMethod, "auth-method", "key", "Auth method: password or key")
	remoteAddCmd.Flags().StringVar(&authPass, "password", "", "SSH password")
	remoteAddCmd.Flags().StringVar(&authKey, "key", "~/.ssh/id_rsa", "SSH private key path")
	remoteAddCmd.Flags().StringVar(&authPhrase, "passphrase", "", "Key passphrase")
	remoteAddCmd.MarkFlagRequired("host")

	// add mapping flags
	addCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	addCmd.Flags().StringVarP(&remoteName, "remote", "r", "", "Remote name (VPS, required)")
	addCmd.Flags().IntVarP(&listenPort, "listen", "l", 0, "Listen port on VPS (required)")
	addCmd.Flags().StringVar(&localHost, "local-host", "127.0.0.1", "Local target host")
	addCmd.Flags().IntVarP(&localPort, "local-port", "p", 0, "Local target port (required)")
	addCmd.MarkFlagRequired("remote")
	addCmd.MarkFlagRequired("listen")
	addCmd.MarkFlagRequired("local-port")

	deleteCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	startCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	stopCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")

	groupCmd.AddCommand(groupStartCmd, groupStopCmd)
	remoteCmd.AddCommand(remoteAddCmd, remoteDeleteCmd)

	rootCmd.AddCommand(serveCmd, remoteCmd, addCmd, deleteCmd, listCmd, startCmd, stopCmd, groupCmd)
	rootCmd.Execute()
}
