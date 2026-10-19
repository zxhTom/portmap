package multipassclient

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestResolveMetadata(t *testing.T) {
	cases := []struct {
		name, data, want string
		bad              bool
	}{
		{"running", `{"info":{"vm":{"state":"Running","ipv4":["127.0.0.1","0.0.0.0","224.0.0.1","10.1.2.3"]}}}`, "10.1.2.3", false},
		{"stopped", `{"info":{"vm":{"state":"Stopped","ipv4":["10.1.2.3"]}}}`, "", true},
		{"no-ip", `{"info":{"vm":{"state":"Running","ipv4":[]}}}`, "", true},
		{"missing", `{"info":{}}`, "", true},
		{"errors", `{"errors":["lookup failed"],"info":{}}`, "", true},
		{"malformed", `not json`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClient("unused")
			c.run = func(_ context.Context, args ...string) ([]byte, error) {
				if !reflect.DeepEqual(args, []string{"info", "--format", "json", "vm"}) {
					t.Fatalf("unexpected command: %v", args)
				}
				return []byte(tc.data), nil
			}
			got, err := c.Resolve(context.Background(), "vm")
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("Resolve = %q, %v", got, err)
			}
		})
	}
}

func TestResolveRejectsOptionsAndPropagatesCancellation(t *testing.T) {
	c := NewClient("unused")
	called := false
	c.run = func(ctx context.Context, _ ...string) ([]byte, error) { called = true; return nil, ctx.Err() }
	if _, err := c.Resolve(context.Background(), "--all"); err == nil || called {
		t.Fatal("option injection accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Resolve(ctx, "vm"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestListInstancesAndCommandFailure(t *testing.T) {
	c := NewClient("unused")
	c.run = func(_ context.Context, args ...string) ([]byte, error) {
		if !reflect.DeepEqual(args, []string{"list", "--format", "json"}) {
			t.Fatalf("unexpected command %v", args)
		}
		return []byte(`{"list":[{"name":"vm","state":"Running","ipv4":["10.1.2.3"],"release":"Ubuntu 24.04"}]}`), nil
	}
	instances, err := c.List(context.Background())
	if err != nil || len(instances) != 1 || instances[0].Name != "vm" {
		t.Fatalf("list = %v %v", instances, err)
	}
	c = NewClient("/nonexistent-portmap-multipass")
	if _, err := c.List(context.Background()); err == nil {
		t.Fatal("missing Multipass binary accepted")
	}
}
