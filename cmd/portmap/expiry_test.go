package main

import (
	"testing"
	"time"
)

func TestExpiryFromFlags(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for _, s := range []string{"7d", "168h"} {
		got, err := expiryFromFlags(s, "", false, now)
		if err != nil || got != now.Add(7*24*time.Hour).Format(time.RFC3339Nano) {
			t.Fatalf("duration %s: %s %v", s, got, err)
		}
	}
	until := now.Add(time.Hour).Format(time.RFC3339)
	if got, err := expiryFromFlags("", until, false, now); err != nil || got != until {
		t.Fatalf("absolute: %s %v", got, err)
	}
	if got, err := expiryFromFlags("", "", true, now); err != nil || got != "" {
		t.Fatal("clear failed")
	}
	for _, s := range []string{"0d", "-1d", "1000000d", "0h", "-2h", "bad"} {
		if _, err := expiryFromFlags(s, "", false, now); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
	if _, err := expiryFromFlags("1h", until, false, now); err == nil {
		t.Fatal("conflicting flags accepted")
	}
	if _, err := expiryFromFlags("", "", false, now); err == nil {
		t.Fatal("missing flags accepted")
	}
	if _, err := expiryFromFlags("", now.Format(time.RFC3339), false, now); err == nil {
		t.Fatal("past deadline accepted")
	}
}
