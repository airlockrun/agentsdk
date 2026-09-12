package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const CallerHeader = "X-Airlock-Caller"

const maxCallerHeaderBytes = 16 << 10

// Caller is host-attributed execution metadata, not an authentication credential.
type Caller struct {
	Kind      string       `json:"kind"`
	Access    Access       `json:"access"`
	User      *CallerUser  `json:"user,omitempty"`
	Initiator *CallerUser  `json:"initiator,omitempty"`
	Origin    CallerOrigin `json:"origin"`
}

type CallerUser struct {
	ID             string `json:"id"`
	Email          string `json:"email"`
	DisplayName    string `json:"displayName"`
	PlatformMember bool   `json:"platformMember"`
}

type CallerOrigin struct {
	Interface string `json:"interface"`
	Platform  string `json:"platform"`
	ClientID  string `json:"clientId"`
	Execution string `json:"execution"`
}

// Validate checks metadata consistency without establishing authority or resolving identity.
func (c Caller) Validate() error {
	switch c.Access {
	case AccessPublic, AccessUser, AccessAdmin:
	default:
		return errors.New("invalid caller access")
	}
	for _, user := range []*CallerUser{c.User, c.Initiator} {
		if user != nil && strings.TrimSpace(user.ID) == "" {
			return errors.New("caller user ID is required")
		}
	}
	switch c.Kind {
	case "anonymous":
		if c.User != nil || c.Initiator != nil || c.Access != AccessPublic {
			return errors.New("anonymous caller requires public access and no user or initiator")
		}
	case "user":
		if c.User == nil || c.Initiator == nil || *c.User != *c.Initiator {
			return errors.New("user caller requires matching user and initiator")
		}
		if c.Access != AccessPublic && !c.User.PlatformMember {
			return errors.New("user or admin access requires platform membership")
		}
	case "application":
		if c.User != nil {
			return errors.New("application caller cannot have an acting user")
		}
	default:
		return errors.New("invalid caller kind")
	}
	switch c.Origin.Interface {
	case "http", "chat", "mcp", "schedule", "application", "unknown":
	default:
		return errors.New("invalid caller interface")
	}
	switch c.Origin.Execution {
	case "request", "job", "background", "startup", "unknown":
	default:
		return errors.New("invalid caller execution")
	}
	return nil
}

func EncodeCallerHeader(c Caller) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	if base64.RawURLEncoding.EncodedLen(len(data)) > maxCallerHeaderBytes {
		return "", errors.New("caller header exceeds 16 KiB")
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func DecodeCallerHeader(headers http.Header) (Caller, error) {
	var value string
	count := 0
	for name, values := range headers {
		if strings.EqualFold(name, CallerHeader) {
			count += len(values)
			if len(values) != 0 {
				value = values[0]
			}
		}
	}
	if count != 1 || value == "" {
		return Caller{}, errors.New("exactly one caller header is required")
	}
	if len(value) > maxCallerHeaderBytes {
		return Caller{}, errors.New("caller header exceeds 16 KiB")
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || base64.RawURLEncoding.EncodeToString(data) != value {
		return Caller{}, errors.New("invalid caller header encoding")
	}
	var c Caller
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Caller{}, fmt.Errorf("invalid caller header: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Caller{}, errors.New("trailing caller header data")
	}
	if err := c.Validate(); err != nil {
		return Caller{}, err
	}
	return c, nil
}
