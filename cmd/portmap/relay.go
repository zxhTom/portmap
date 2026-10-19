package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"github.com/zxhtom/portmap/internal/relay"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var relayDir, relayAddr, relaySocket, relayZone string

func relayDataDir() string {
	if relayDir != "" {
		return relayDir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "portmap-relay")
}
func relaySocketPath() string {
	if relaySocket != "" {
		return relaySocket
	}
	return filepath.Join(relayDataDir(), "control.sock")
}

var relayCmd = &cobra.Command{Use: "relay", Short: "Central domain ownership and HTTP routing service on the VPS"}
var relayServeCmd = &cobra.Command{Use: "serve", RunE: func(cmd *cobra.Command, args []string) error {
	s, err := relay.New(relayDataDir(), relayZone)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Listen(relayAddr, relaySocketPath()); err != nil {
		return err
	}
	fmt.Println("Portmap domain relay started on", relayAddr, "zone", relayZone)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(ch)
	<-ch
	return nil
}}
var relayRequestCmd = &cobra.Command{Use: "request", Short: "Send one JSON control request over the local private Unix socket", RunE: func(cmd *cobra.Command, args []string) error {
	var req relayprotocol.Request
	d := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	response, err := relay.Request(ctx, relaySocketPath(), req)
	if err != nil {
		response.Error = err.Error()
	}
	json.NewEncoder(os.Stdout).Encode(response)
	return nil
}}

func init() {
	relayCmd.PersistentFlags().StringVar(&relayDir, "relay-data-dir", "", "Relay data directory")
	relayCmd.PersistentFlags().StringVar(&relaySocket, "socket", "", "Private relay control socket path")
	relayServeCmd.Flags().StringVar(&relayAddr, "listen", "127.0.0.1:9881", "Loopback HTTP listener for Nginx")
	relayServeCmd.Flags().StringVar(&relayZone, "zone", relayprotocol.DefaultZone, "Allowed domain suffix")
	relayCmd.AddCommand(relayServeCmd, relayRequestCmd)
	rootCmd.AddCommand(relayCmd)
}
