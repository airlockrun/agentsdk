package main

import (
	"strings"
	"testing"
)

func TestResourceCommandFamily(t *testing.T) {
	for _, family := range []string{"integrations", "connection", "mcp"} {
		t.Run(family, func(t *testing.T) {
			if err := run([]string{family}); err == nil || !strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("redundant resource command family exposed: %v", err)
			}
		})
	}
}

func TestResourceAddress(t *testing.T) {
	for _, tt := range []struct {
		value, global, target, slot string
		bad                         bool
	}{
		{value: "/personal_spotify", global: "personal_spotify"},
		{value: "/11111111-1111-4111-8111-111111111111", global: "11111111-1111-4111-8111-111111111111"},
		{value: "/preview", global: "preview"},
		{value: "/production", global: "production"},
		{value: "/airlock", global: "airlock"},
		{value: "preview/spotify", target: "preview", slot: "spotify"},
		{value: "production/spotify", target: "production", slot: "spotify"},
		{value: "staging/spotify", target: "staging", slot: "spotify"},
		{value: "spotify", bad: true}, {value: "preview", bad: true}, {value: "//spotify", bad: true}, {value: "/", bad: true}, {value: "preview/", bad: true}, {value: "preview/spotify/extra", bad: true}, {value: "../spotify", bad: true}, {value: "/spotify?token=x", bad: true},
		{value: "/name%2Fslot", bad: true}, {value: "preview/..", bad: true}, {value: "preview/slot#fragment", bad: true}, {value: " preview/slot", bad: true}, {value: "preview/slot\\extra", bad: true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			got, err := parseResourceAddress(tt.value)
			if (err != nil) != tt.bad {
				t.Fatalf("error=%v", err)
			}
			if err == nil && (got.global != tt.global || got.target != tt.target || got.slot != tt.slot) {
				t.Fatalf("address=%+v", got)
			}
		})
	}
	f := targetFlags{target: "production"}
	if err := selectResourceTarget(&f, "preview"); err == nil {
		t.Fatal("conflicting target overridden")
	}
}
