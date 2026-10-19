package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zxhtom/portmap/internal/webui"
)

var webPasswordCmd = &cobra.Command{Use: "web-password", Short: "Reset the web administrator password from stdin (restart service to apply)", RunE: func(cmd *cobra.Command, args []string) error {
	input, _ := cmd.Flags().GetBool("password-stdin")
	if !input {
		return fmt.Errorf("use --password-stdin and provide a 12–72 byte password on standard input")
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 1024))
	if err != nil {
		return err
	}
	if err := webui.ResetPassword(dataDirPath(), strings.TrimRight(string(b), "\r\n")); err != nil {
		return err
	}
	fmt.Println("Web password reset. Restart portmap.service to apply and invalidate existing sessions.")
	return nil
}}

func init() {
	webPasswordCmd.Flags().Bool("password-stdin", false, "Read a new password from standard input")
	rootCmd.AddCommand(webPasswordCmd)
}
