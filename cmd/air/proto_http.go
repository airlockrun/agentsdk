package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	protoMarshal   = protojson.MarshalOptions{UseProtoNames: false, EmitUnpopulated: true}
	protoUnmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}
	apiClient      = &http.Client{Timeout: 11 * time.Minute}
)

type httpStatusError struct {
	StatusCode int
	Status     string
	Message    string
}

func (e *httpStatusError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return fmt.Sprintf("%s: %s", e.Status, e.Message)
}

func isAuthRejected(err error) bool {
	return hasHTTPStatus(err, http.StatusUnauthorized) || hasHTTPStatus(err, http.StatusForbidden)
}

func hasHTTPStatus(err error, code int) bool {
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.StatusCode == code
}

func doProto(ctx context.Context, baseURL, method, path, token string, in proto.Message, out proto.Message) error {
	var body io.Reader
	if in != nil {
		b, err := protoMarshal.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, normalizeBaseURL(baseURL)+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newHTTPStatusError(resp.StatusCode, resp.Status, bodyBytes)
	}
	if out == nil {
		return nil
	}
	return protoUnmarshal.Unmarshal(bodyBytes, out)
}

func newHTTPStatusError(statusCode int, status string, body []byte) error {
	var er airlockv1.ErrorResponse
	if err := protoUnmarshal.Unmarshal(body, &er); err == nil && er.Error != "" {
		return &httpStatusError{StatusCode: statusCode, Status: status, Message: er.Error}
	}
	return &httpStatusError{StatusCode: statusCode, Status: status, Message: strings.TrimSpace(string(body))}
}
