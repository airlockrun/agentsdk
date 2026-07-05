package main

import (
	"bytes"
	"context"
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
	apiClient      = &http.Client{Timeout: 10 * time.Minute}
)

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
		var er airlockv1.ErrorResponse
		if err := protoUnmarshal.Unmarshal(bodyBytes, &er); err == nil && er.Error != "" {
			return fmt.Errorf("%s: %s", resp.Status, er.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(bodyBytes)))
	}
	if out == nil {
		return nil
	}
	return protoUnmarshal.Unmarshal(bodyBytes, out)
}
