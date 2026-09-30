package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	StatusCode  int
	Status      string
	Message     string
	Detail      string
	AccessToken string
}

func (e *httpStatusError) Error() string {
	if e.Message == "" {
		return e.Status
	}
	return fmt.Sprintf("%s: %s", e.Status, e.Message)
}

func isTerminalSessionError(err error) bool {
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	switch statusErr.Detail {
	case "session_expired", "session_revoked", "session_auth_epoch_changed", "invalid_refresh_token":
		return true
	default:
		return false
	}
}

func hasHTTPStatus(err error, code int) bool {
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	return statusErr.StatusCode == code
}

func doProto(ctx context.Context, baseURL, method, path, token string, in proto.Message, out proto.Message) error {
	var body []byte
	if in != nil {
		b, err := protoMarshal.Marshal(in)
		if err != nil {
			return err
		}
		body = b
	}
	err := doProtoOnce(ctx, baseURL, method, path, token, body, in != nil, out)
	if err == nil || token == "" || !hasHTTPStatus(err, http.StatusUnauthorized) {
		return err
	}
	refreshed, refreshErr := accessTokenForURLAfterRejection(ctx, baseURL, token)
	if errors.Is(refreshErr, errTokenNotManaged) || errors.Is(refreshErr, errRefreshUnavailable) {
		return err
	}
	if refreshErr != nil {
		return refreshErr
	}
	return doProtoOnce(ctx, baseURL, method, path, refreshed, body, in != nil, out)
}

func doProtoOnce(ctx context.Context, baseURL, method, path, token string, body []byte, hasBody bool, out proto.Message) error {
	var reader io.Reader
	if hasBody {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, normalizeBaseURL(baseURL)+path, reader)
	if err != nil {
		return err
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := requestClient(req, token).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := newHTTPStatusError(resp.StatusCode, resp.Status, bodyBytes)
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) {
			statusErr.AccessToken = token
		}
		return err
	}
	if out == nil {
		return nil
	}
	return protoUnmarshal.Unmarshal(bodyBytes, out)
}

func rejectedAccessToken(err error, fallback string) string {
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.AccessToken != "" {
		return statusErr.AccessToken
	}
	return fallback
}

func newHTTPStatusError(statusCode int, status string, body []byte) error {
	var er airlockv1.ErrorResponse
	if err := protoUnmarshal.Unmarshal(body, &er); err == nil && er.Error != "" {
		return &httpStatusError{StatusCode: statusCode, Status: status, Message: er.Error, Detail: er.Detail}
	}
	return &httpStatusError{StatusCode: statusCode, Status: status, Message: strings.TrimSpace(string(body))}
}

func requestClient(req *http.Request, token string) *http.Client {
	if token == "" {
		return apiClient
	}
	origin, originErr := requestOrigin(req.URL)
	client := *apiClient
	previous := client.CheckRedirect
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		nextOrigin, err := requestOrigin(next.URL)
		if originErr != nil || err != nil || nextOrigin != origin {
			return errors.New("refusing to redirect an authenticated request to a different origin")
		}
		if previous != nil {
			return previous(next, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

func requestOrigin(u *url.URL) (string, error) {
	if u == nil || u.User != nil {
		return "", errors.New("request URL has no safe origin")
	}
	return canonicalAuthOrigin(u.Scheme + "://" + u.Host)
}

func doAuthenticatedHTTP(ctx context.Context, baseURL, token string, newRequest func(string) (*http.Request, error)) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		req, err := newRequest(token)
		if err != nil {
			return nil, err
		}
		resp, err := requestClient(req, token).Do(req)
		if err != nil {
			return nil, err
		}
		if attempt != 0 || resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		refreshed, refreshErr := accessTokenForURLAfterRejection(ctx, baseURL, token)
		if errors.Is(refreshErr, errTokenNotManaged) || errors.Is(refreshErr, errRefreshUnavailable) {
			return resp, nil
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if refreshErr != nil {
			return nil, refreshErr
		}
		token = refreshed
	}
	panic("unreachable")
}
