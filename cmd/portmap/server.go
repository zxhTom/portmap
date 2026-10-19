package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/zxhtom/portmap/internal/relay"
	"github.com/zxhtom/portmap/internal/relayprotocol"
	"github.com/zxhtom/portmap/internal/serverui"
)

var serverWebAddr, serverWebCert, serverWebKey string

var serverCmd = &cobra.Command{Use: "server", Short: "Run and inspect the VPS-side portmap service"}

var serverServeCmd = &cobra.Command{
	Use: "serve", Short: "Run the domain relay and VPS tunnel console",
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := relay.New(relayDataDir(), relayZone)
		if err != nil {
			return err
		}
		defer s.Close()
		ui, err := serverui.New(relayDataDir(), s.Inventory())
		if err != nil {
			return err
		}
		if err := ui.Listen(serverWebAddr, serverWebCert, serverWebKey); err != nil {
			return err
		}
		defer ui.Close()
		if err := s.Listen(relayAddr, relaySocketPath()); err != nil {
			return err
		}
		fmt.Printf("Portmap server started: relay %s, console %s, zone %s\n", relayAddr, serverWebAddr, relayZone)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(ch)
		<-ch
		return nil
	},
}

var serverRequestCmd = &cobra.Command{
	Use: "request", Short: "Send an SSH-authenticated request to the private VPS control socket",
	RunE: func(cmd *cobra.Command, args []string) error {
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
		return json.NewEncoder(os.Stdout).Encode(response)
	},
}

func init() {
	serverCmd.PersistentFlags().StringVar(&relayDir, "relay-data-dir", "", "Server data directory")
	serverCmd.PersistentFlags().StringVar(&relaySocket, "socket", "", "Private server control socket path")
	serverServeCmd.Flags().StringVar(&relayAddr, "listen", "127.0.0.1:9881", "Loopback HTTP relay listener for Nginx")
	serverServeCmd.Flags().StringVar(&relayZone, "zone", relayprotocol.DefaultZone, "Allowed domain suffix")
	serverServeCmd.Flags().StringVar(&serverWebAddr, "web-listen", "127.0.0.1:9890", "VPS console listener")
	serverServeCmd.Flags().StringVar(&serverWebCert, "web-cert", "", "VPS console TLS certificate")
	serverServeCmd.Flags().StringVar(&serverWebKey, "web-key", "", "VPS console TLS private key")
	serverCmd.AddCommand(serverServeCmd, serverRequestCmd)
	rootCmd.AddCommand(serverCmd)
}
