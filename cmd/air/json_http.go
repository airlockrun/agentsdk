package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

func doJSON(ctx context.Context, baseURL, method, path, token string, in, out any) error {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
		if err != nil {
			return err
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		var reader io.Reader
		if in != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, normalizeBaseURL(baseURL)+path, reader)
		if err != nil {
			return err
		}
		if in != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := requestClient(req, token).Do(req)
		if err != nil {
			return err
		}
		responseBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && token != "" {
			refreshed, refreshErr := accessTokenForURLAfterRejection(ctx, baseURL, token)
			if refreshErr == nil {
				token = refreshed
				continue
			}
			if !errors.Is(refreshErr, errTokenNotManaged) && !errors.Is(refreshErr, errRefreshUnavailable) {
				return refreshErr
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return newHTTPStatusError(resp.StatusCode, resp.Status, responseBody)
		}
		if out == nil || len(responseBody) == 0 {
			return nil
		}
		return json.Unmarshal(responseBody, out)
	}
	panic("unreachable")
}
