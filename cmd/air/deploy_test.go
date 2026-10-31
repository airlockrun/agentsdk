package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestCreateDraftAgentSlugIntent(t *testing.T) {
	for _, tt := range []struct {
		name, slug string
		intent     airlockv1.AppSlugIntent
	}{
		{name: "Каталог товаров", intent: airlockv1.AppSlugIntent_APP_SLUG_INTENT_GENERATED},
		{name: "🚀", intent: airlockv1.AppSlugIntent_APP_SLUG_INTENT_GENERATED},
		{name: "Explicit address", slug: "catalog", intent: airlockv1.AppSlugIntent_APP_SLUG_INTENT_EXPLICIT},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"--create", "--name", tt.name, "-m", "Initial implementation"}
			if tt.slug != "" {
				args = append(args, "--slug", tt.slug)
			}
			flags, err := parseDeployFlags(args)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request airlockv1.CreateAgentRequest
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				if err := protojson.Unmarshal(body, &request); err != nil {
					t.Error(err)
				}
				if request.SlugIntent != tt.intent || request.Name != tt.name || request.Slug != tt.slug || !request.SkipInitialBuild {
					t.Errorf("request=%+v", &request)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"agent":{"id":"11111111-1111-4111-8111-111111111111","slug":"server-selected-2"}}`))
			}))
			defer server.Close()
			binding, err := createDraftAgent(t.Context(), server.URL, "token", flags)
			if err != nil || binding.Slug != "server-selected-2" {
				t.Fatalf("binding=%+v err=%v", binding, err)
			}
		})
	}
}
