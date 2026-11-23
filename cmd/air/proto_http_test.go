package main

import (
	"strings"
	"testing"
)

func TestHTTPStatusErrorRedactsTokenWithoutLosingRefreshIdentity(t *testing.T) {
	for _, message := range []string{"", "server echoed private-access-token"} {
		t.Run(message, func(t *testing.T) {
			err := &httpStatusError{StatusCode: 401, Status: "401 private-access-token", Message: message, AccessToken: "private-access-token"}
			if strings.Contains(err.Error(), "private-access-token") || !strings.Contains(err.Error(), "[redacted]") {
				t.Fatalf("credential exposed in error: %s", err.Error())
			}
			if rejectedAccessToken(err, "fallback") != "private-access-token" || !hasHTTPStatus(err, 401) {
				t.Fatal("error rendering changed authentication identity/status")
			}
		})
	}
}
