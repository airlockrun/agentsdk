package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestOperationChallengeUsesMachineChannel(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	socket := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("SOL_COMMAND_CONTROL_SOCKET", socket)
	reported := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		var signal struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(conn).Decode(&signal) != nil {
			return
		}
		reported <- signal.ID
		_ = json.NewEncoder(conn).Encode(map[string]bool{"accepted": true})
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Airlock-Operation-Challenge", id)
		w.WriteHeader(428)
	}))
	defer server.Close()
	client := &http.Client{Transport: approvalTransport{next: http.DefaultTransport}}
	_, err = client.Get(server.URL)
	var approval *approvalRequiredError
	if !errors.As(err, &approval) || approval.id != id {
		t.Fatal("typed challenge lost", err)
	}
	select {
	case got := <-reported:
		if got != id {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("no structured supervisor report")
	}
}

func TestStandaloneOperationChallengeDoesNotWaitForHuman(t *testing.T) {
	t.Setenv("SOL_COMMAND_CONTROL_SOCKET", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Airlock-Operation-Challenge", "11111111-1111-4111-8111-111111111111")
		w.WriteHeader(428)
	}))
	defer server.Close()
	client := &http.Client{Transport: approvalTransport{next: http.DefaultTransport}}
	_, err := client.Get(server.URL)
	var approval *approvalRequiredError
	if !errors.As(err, &approval) {
		t.Fatal(err)
	}
}
