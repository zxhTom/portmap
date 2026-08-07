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
	bindAddr   string
	logFile    string
	group      string
	localPort  int
	targetHost string
	targetPort int
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

func client() *http.Client {
	return &http.Client{}
}

func apiURL(path string) string {
	return "http://" + apiAddr + path
}

var rootCmd = &cobra.Command{
	Use:   "portmap",
	Short: "Port mapping manager",
}

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
			return fmt.Errorf("api listen %s: %w", apiAddr, err)
		}

		slog.Info("portmap daemon started", "data_dir", dir, "api", apiAddr)
		if bindAddr != "" {
			mgr.SetBindAddr(bindAddr)
		}

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		slog.Info("shutting down...")
		srv.Close()
		mgr.Shutdown()
		return nil
	},
}

var addCmd = &cobra.Command{
	Use:   "add <name>",
	Short: "Add a port mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req := map[string]interface{}{
			"group":       group,
			"name":        args[0],
			"local_port":  localPort,
			"target_host": targetHost,
			"target_port": targetPort,
		}
		return apiPost("/api/mappings", req)
	},
}

var updateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Update a port mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		req := map[string]interface{}{
			"group":       group,
			"local_port":  localPort,
			"target_host": targetHost,
			"target_port": targetPort,
		}
		return apiPut(fmt.Sprintf("/api/mappings/%s", args[0]), req)
	},
}

var deleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a port mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := fmt.Sprintf("/api/mappings/%s?group=%s", args[0], group)
		return apiDelete(url)
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all port mappings",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := client().Get(apiURL("/api/mappings"))
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
		fmt.Fprintln(w, "GROUP\tNAME\tSTATUS\tLOCAL_PORT\tTARGET\tSTARTED")
		for _, m := range mappings {
			target := fmt.Sprintf("%s:%d", m.TargetHost, m.TargetPort)
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
				m.Group, m.Name, m.Status, m.LocalPort, target, m.StartedAt)
		}
		return w.Flush()
	},
}

var startCmd = &cobra.Command{
	Use:   "start <name>",
	Short: "Start a port mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := fmt.Sprintf("/api/mappings/%s/start?group=%s", args[0], group)
		return apiPost(url, nil)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop <name>",
	Short: "Stop a port mapping",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url := fmt.Sprintf("/api/mappings/%s/stop?group=%s", args[0], group)
		return apiPost(url, nil)
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

var groupCmd = &cobra.Command{
	Use:   "group",
	Short: "Group management",
}

func apiPost(url string, body interface{}) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	resp, err := client().Post(apiURL(url), "application/json", r)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", body)
	}
	if body != nil {
		io.Copy(os.Stdout, resp.Body)
		fmt.Println()
	}
	return nil
}

func apiPut(url string, body interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, apiURL(url), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", body)
	}
	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
	return nil
}

func apiDelete(url string) error {
	req, err := http.NewRequest(http.MethodDelete, apiURL(url), nil)
	if err != nil {
		return err
	}
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", body)
	}
	io.Copy(os.Stdout, resp.Body)
	fmt.Println()
	return nil
}

func main() {
	rootCmd.PersistentFlags().StringVar(&dataDir, "data-dir", "", "Data directory (default ~/.config/portmap)")
	rootCmd.PersistentFlags().StringVar(&apiAddr, "api-addr", "127.0.0.1:9876", "API server address")

	serveCmd.Flags().StringVar(&bindAddr, "bind-addr", "", "Bind address for port listeners (default from config)")
	serveCmd.Flags().StringVar(&logFile, "log-file", "", "Log file path (default stderr only)")

	addCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	addCmd.Flags().IntVarP(&localPort, "port", "p", 0, "Local port (0=auto)")
	addCmd.Flags().StringVar(&targetHost, "target-host", "", "Target host")
	addCmd.Flags().IntVar(&targetPort, "target-port", 0, "Target port")
	addCmd.MarkFlagRequired("target-host")
	addCmd.MarkFlagRequired("target-port")

	updateCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	updateCmd.Flags().IntVarP(&localPort, "port", "p", 0, "Local port (0=unchanged)")
	updateCmd.Flags().StringVar(&targetHost, "target-host", "", "Target host")
	updateCmd.Flags().IntVar(&targetPort, "target-port", 0, "Target port")

	deleteCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	startCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	stopCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")

	groupCmd.AddCommand(groupStartCmd)
	groupCmd.AddCommand(groupStopCmd)

	rootCmd.AddCommand(serveCmd, addCmd, updateCmd, deleteCmd, listCmd, startCmd, stopCmd, groupCmd)
	rootCmd.Execute()
}
