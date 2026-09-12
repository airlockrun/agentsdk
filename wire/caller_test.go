package wire

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func validCaller() Caller {
	user := &CallerUser{ID: "user-id", Email: "alice@example.com", DisplayName: "Alice", PlatformMember: true}
	return Caller{Kind: "user", Access: AccessUser, User: user, Initiator: user,
		Origin: CallerOrigin{Interface: "chat", Platform: "slack", ClientID: "client-id", Execution: "request"}}
}

func TestCallerValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Caller)
		valid  bool
	}{
		{"user", func(*Caller) {}, true},
		{"public member", func(c *Caller) { c.Access = AccessPublic }, true},
		{"public nonmember metadata", func(c *Caller) { c.Access = AccessPublic; c.User.PlatformMember = false }, true},
		{"admin user", func(c *Caller) { c.Access = AccessAdmin }, true},
		{"anonymous", func(c *Caller) { c.Kind = "anonymous"; c.Access = AccessPublic; c.User = nil; c.Initiator = nil }, true},
		{"application admin", func(c *Caller) { c.Kind = "application"; c.Access = AccessAdmin; c.User = nil; c.Initiator = nil }, true},
		{"application user access", func(c *Caller) { c.Kind = "application"; c.User = nil; c.Initiator = nil }, true},
		{"explicit unknown origin", func(c *Caller) { c.Origin = CallerOrigin{Interface: "unknown", Execution: "unknown"} }, true},
		{"missing kind", func(c *Caller) { c.Kind = "" }, false},
		{"unknown kind", func(c *Caller) { c.Kind = "external" }, false},
		{"missing access", func(c *Caller) { c.Access = "" }, false},
		{"unknown access", func(c *Caller) { c.Access = "owner" }, false},
		{"missing user", func(c *Caller) { c.User = nil }, false},
		{"empty ID", func(c *Caller) { c.User.ID = "" }, false},
		{"whitespace ID", func(c *Caller) { c.User.ID = " \t" }, false},
		{"missing initiator", func(c *Caller) { c.Initiator = nil }, false},
		{"mismatched initiator", func(c *Caller) { user := *c.User; user.Email = "other@example.com"; c.Initiator = &user }, false},
		{"nonmember user access", func(c *Caller) { c.User.PlatformMember = false }, false},
		{"nonmember admin access", func(c *Caller) { c.User.PlatformMember = false; c.Access = AccessAdmin }, false},
		{"anonymous user", func(c *Caller) { c.Kind = "anonymous"; c.Access = AccessPublic }, false},
		{"anonymous initiator", func(c *Caller) { c.Kind = "anonymous"; c.Access = AccessPublic; c.User = nil }, false},
		{"anonymous admin", func(c *Caller) { c.Kind = "anonymous"; c.Access = AccessAdmin; c.User = nil; c.Initiator = nil }, false},
		{"application acting user", func(c *Caller) { c.Kind = "application" }, false},
		{"missing interface", func(c *Caller) { c.Origin.Interface = "" }, false},
		{"unknown interface", func(c *Caller) { c.Origin.Interface = "web" }, false},
		{"missing execution", func(c *Caller) { c.Origin.Execution = "" }, false},
		{"unknown execution", func(c *Caller) { c.Origin.Execution = "tool" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validCaller()
			tc.change(&c)
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("Validate() = %v, valid=%t", err, tc.valid)
			}
			_, err := EncodeCallerHeader(c)
			if (err == nil) != tc.valid {
				t.Fatalf("EncodeCallerHeader() = %v, valid=%t", err, tc.valid)
			}
		})
	}
	for _, surface := range []string{"http", "chat", "mcp", "schedule", "application", "unknown"} {
		for _, execution := range []string{"request", "job", "background", "startup", "unknown"} {
			t.Run(surface+"/"+execution, func(t *testing.T) {
				c := validCaller()
				c.Origin.Interface = surface
				c.Origin.Execution = execution
				if err := c.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestCallerHeaderRoundTrip(t *testing.T) {
	for _, member := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonmember metadata", true: "member"}[member], func(t *testing.T) {
			c := validCaller()
			c.Access = AccessPublic
			c.User.PlatformMember = member
			value, err := EncodeCallerHeader(c)
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodeCallerHeader(http.Header{"x-aIrLoCk-cAlLeR": {value}})
			if err != nil || !reflect.DeepEqual(got, c) {
				t.Fatalf("round trip = %+v, %v; want %+v", got, err, c)
			}
			data, err := base64.RawURLEncoding.DecodeString(value)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"platformMember":`) {
				t.Fatal("membership must be explicit")
			}
		})
	}
}

func TestDecodeCallerHeaderRejectsInvalidInput(t *testing.T) {
	value, err := EncodeCallerHeader(validCaller())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(validCaller())
	if err != nil {
		t.Fatal(err)
	}
	encode := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, headers := range map[string]http.Header{
		"missing":                    nil,
		"empty":                      {CallerHeader: {""}},
		"duplicate":                  {CallerHeader: {value, value}},
		"case insensitive duplicate": {CallerHeader: {value}, "x-airlock-caller": {value}},
		"comma joined":               {CallerHeader: {value + "," + value}},
		"oversized":                  {CallerHeader: {strings.Repeat("a", maxCallerHeaderBytes+1)}},
		"invalid base64":             {CallerHeader: {"!"}},
		"padding":                    {CallerHeader: {value + "="}},
		"newline":                    {CallerHeader: {value + "\n"}},
		"invalid JSON":               {CallerHeader: {encode("{")}},
		"null":                       {CallerHeader: {encode("null")}},
		"empty object":               {CallerHeader: {encode("{}")}},
		"unknown field":              {CallerHeader: {encode(strings.TrimSuffix(string(data), "}") + `,"credential":"secret"}`)}},
		"unknown nested field":       {CallerHeader: {encode(strings.Replace(string(data), `"id":`, `"provider":"example","id":`, 1))}},
		"trailing JSON":              {CallerHeader: {encode(string(data) + "{}")}},
		"trailing garbage":           {CallerHeader: {encode(string(data) + "garbage")}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCallerHeader(headers); err == nil {
				t.Fatal("accepted invalid caller header")
			}
		})
	}
}

func TestCallerHeaderSizeBound(t *testing.T) {
	c := validCaller()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Origin.Platform += strings.Repeat("x", maxCallerHeaderBytes*3/4-len(data))
	value, err := EncodeCallerHeader(c)
	if err != nil || len(value) != maxCallerHeaderBytes {
		t.Fatalf("boundary len=%d err=%v", len(value), err)
	}
	if _, err := DecodeCallerHeader(http.Header{CallerHeader: {value}}); err != nil {
		t.Fatal(err)
	}
	c.Origin.Platform += "x"
	if _, err := EncodeCallerHeader(c); err == nil {
		t.Fatal("encoded oversized header")
	}
}
