package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"io"
	"net/http"
	"net/url"
)

var bindPrefixes []string
var bindScheme string
var domainsCmd = &cobra.Command{Use: "domains <name>", Short: "Set domain prefix(es) or clear domain routing (brief reconnect)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	clear, _ := cmd.Flags().GetBool("clear")
	if clear && len(bindPrefixes) > 0 {
		return fmt.Errorf("use --prefix or --clear, not both")
	}
	if !clear && len(bindPrefixes) == 0 {
		return fmt.Errorf("specify --prefix (repeatable) or --clear")
	}
	if clear {
		bindPrefixes = []string{}
	}
	body, _ := json.Marshal(map[string]any{"domain_prefixes": bindPrefixes, "upstream_scheme": bindScheme, "domain_zone": cmd.Flag("domain-zone").Value.String()})
	req, err := http.NewRequest(http.MethodPut, apiURL("/api/mappings/"+url.PathEscape(args[0])+"/domains?group="+url.QueryEscape(group)), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s", data)
	}
	fmt.Println("Domain bindings updated. Use portmap list to inspect the URL and status.")
	return nil
}}

func init() {
	domainsCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	domainsCmd.Flags().StringSliceVar(&bindPrefixes, "prefix", nil, "Domain prefixes, repeatable or comma-separated")
	domainsCmd.Flags().String("domain-zone", "", "Suffix bound to this mapping VPS")
	domainsCmd.Flags().StringVar(&bindScheme, "upstream-scheme", "http", "Upstream protocol: http or https")
	domainsCmd.Flags().Bool("clear", false, "Remove domain bindings and return to public TCP forwarding")
	rootCmd.AddCommand(domainsCmd)
}
