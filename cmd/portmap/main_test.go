package main

import (
	"bytes"
	"testing"
)

func TestCLI_Help(t *testing.T) {
	b := new(bytes.Buffer)
	rootCmd.SetOut(b)
	rootCmd.SetErr(b)
	rootCmd.SetArgs([]string{"--help"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd --help failed: %v", err)
	}

	out := b.String()
	if !bytes.Contains([]byte(out), []byte("multipass")) {
		t.Fatalf("missing multipass command: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("docker")) {
		t.Fatalf("expected docker command in root help output, got: %s", out)
	}
}

func TestCLI_AddHelp(t *testing.T) {
	b := new(bytes.Buffer)
	rootCmd.SetOut(b)
	rootCmd.SetErr(b)
	rootCmd.SetArgs([]string{"add", "--help"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("add --help failed: %v", err)
	}

	out := b.String()
	if !bytes.Contains([]byte(out), []byte("--instance-port")) {
		t.Fatalf("missing instance port option: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("--container")) {
		t.Fatalf("expected --container in add help, got: %s", out)
	}
	if !bytes.Contains([]byte(out), []byte("--type")) {
		t.Fatalf("expected --type in add help, got: %s", out)
	}
}

func TestCLI_ServeHelp(t *testing.T) {
	b := new(bytes.Buffer)
	rootCmd.SetOut(b)
	rootCmd.SetErr(b)
	rootCmd.SetArgs([]string{"serve", "--help"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("serve --help failed: %v", err)
	}

	out := b.String()
	if !bytes.Contains([]byte(out), []byte("--docker-host")) {
		t.Fatalf("expected --docker-host in serve help, got: %s", out)
	}
}
