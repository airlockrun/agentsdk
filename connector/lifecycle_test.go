package connector

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestOnStartRunsInRegistrationOrder(t *testing.T) {
	runtime := New(Config{Kind: "hooks", Contract: DefineContract("io.airlockrun.hooks"), Name: "Hooks", Description: "Startup hooks.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}})
	var calls []string
	runtime.OnStart("first", func(context.Context) error { calls = append(calls, "first"); return nil })
	runtime.OnStart("second", func(context.Context) error { calls = append(calls, "second"); return nil })
	if err := runtime.runStartHooks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"first", "second"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestOnStartFailsLoudly(t *testing.T) {
	runtime := New(Config{Kind: "hooks", Contract: DefineContract("io.airlockrun.hook_errors"), Name: "Hooks", Description: "Startup hooks.", ArtifactVersion: "1", Targets: []string{PlatformLinuxAMD64}})
	want := errors.New("unavailable")
	runtime.OnStart("dependency", func(context.Context) error { return want })
	if err := runtime.runStartHooks(context.Background()); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	assertPanicContains(t, "duplicate OnStart", func() { runtime.OnStart("dependency", func(context.Context) error { return nil }) })
}
