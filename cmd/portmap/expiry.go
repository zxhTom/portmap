package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var addExpiresAt string

func displayExpiry(s string) string {
	if s == "" {
		return "长期"
	}
	return s
}
func displayInvalidReason(s string) string {
	switch s {
	case "expired":
		return "已到期"
	case "connection_failures":
		return "连续连接失败"
	}
	return "-"
}

func expiryFromFlags(duration, until string, clear bool, now time.Time) (string, error) {
	count := 0
	if duration != "" {
		count++
	}
	if until != "" {
		count++
	}
	if clear {
		count++
	}
	if count != 1 {
		return "", fmt.Errorf("specify exactly one of --for, --until, --clear")
	}
	if clear {
		return "", nil
	}
	if duration != "" {
		var d time.Duration
		var err error
		if strings.HasSuffix(duration, "d") {
			var days int
			days, err = strconv.Atoi(strings.TrimSuffix(duration, "d"))
			if days <= 0 || days > 36500 {
				return "", fmt.Errorf("days must be between 1 and 36500")
			}
			d = time.Duration(days) * 24 * time.Hour
		} else {
			d, err = time.ParseDuration(duration)
		}
		if err != nil || d <= 0 {
			return "", fmt.Errorf("invalid positive duration; examples: 24h, 7d")
		}
		return now.Add(d).Format(time.RFC3339Nano), nil
	}
	t, err := time.Parse(time.RFC3339Nano, until)
	if err != nil || !t.After(now) {
		return "", fmt.Errorf("--until requires a future RFC3339 timestamp with timezone")
	}
	return t.Format(time.RFC3339Nano), nil
}

var renewCmd = &cobra.Command{Use: "renew <name>", Aliases: []string{"expiry"}, Short: "Set/renew mapping validity or remove its expiry", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	duration, _ := cmd.Flags().GetString("for")
	until, _ := cmd.Flags().GetString("until")
	clear, _ := cmd.Flags().GetBool("clear")
	deadline, err := expiryFromFlags(duration, until, clear, time.Now())
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"expires_at": deadline})
	req, err := http.NewRequest(http.MethodPut, apiURL("/api/mappings/"+url.PathEscape(args[0])+"/expiry?group="+url.QueryEscape(group)), bytes.NewReader(data))
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
	fmt.Printf("有效期已更新：%s\n", displayExpiry(deadline))
	return nil
}}

func init() {
	renewCmd.Flags().StringVarP(&group, "group", "g", "default", "Group name")
	renewCmd.Flags().String("for", "", "Validity from now, e.g. 24h or 7d")
	renewCmd.Flags().String("until", "", "Absolute RFC3339 deadline with timezone")
	renewCmd.Flags().Bool("clear", false, "Remove expiry (unlimited)")
}
