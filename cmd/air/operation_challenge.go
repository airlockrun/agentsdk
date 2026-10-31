package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

type approvalRequiredError struct{ id string }

func (e *approvalRequiredError) Error() string {
	return "approval_required: " + e.id + "; protected operation was not executed; do not replay the whole shell command"
}

type approvalTransport struct{ next http.RoundTripper }

func (t approvalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	id := response.Header.Get("Airlock-Operation-Challenge")
	if response.StatusCode != http.StatusPreconditionRequired || id == "" {
		return response, nil
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || id != parsed.String() {
		response.Body.Close()
		return nil, errors.New("invalid server operation challenge")
	}
	response.Body.Close()
	if socket := os.Getenv("SOL_COMMAND_CONTROL_SOCKET"); socket != "" {
		conn, err := net.DialTimeout("unix", socket, time.Second)
		if err != nil {
			return nil, errors.Join(&approvalRequiredError{id}, errors.New("command supervisor unavailable"))
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if err := json.NewEncoder(conn).Encode(map[string]string{"id": id}); err != nil {
			return nil, errors.Join(&approvalRequiredError{id}, err)
		}
		var ack struct {
			Accepted bool `json:"accepted"`
		}
		if err := json.NewDecoder(io.LimitReader(conn, 512)).Decode(&ack); err != nil || !ack.Accepted {
			return nil, errors.Join(&approvalRequiredError{id}, errors.New("command supervisor did not acknowledge challenge"))
		}
	}
	return nil, &approvalRequiredError{id}
}
