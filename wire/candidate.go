package wire

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"regexp"
)

const CandidateConfigHeader = "X-Airlock-Candidate-Config"
const ExpectedSourceRevisionHeader = "X-Airlock-Expected-Source-Revision"

// CandidateConfig selects resources from the candidate's own declarations.
// Source admission stores the plan; only the host commits it at candidate cutover.
type CandidateConfig struct {
	Version    int                  `json:"version"`
	Selections []CandidateSelection `json:"selections"`
}
type CandidateSelection struct {
	Type        string `json:"type"`
	Slug        string `json:"slug"`
	ResourceID  string `json:"resourceId,omitempty"`
	FixtureName string `json:"fixtureName,omitempty"`
}

var candidateName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)

func (c CandidateConfig) HeaderValue() (string, error) {
	if c.Version != 1 || len(c.Selections) > 64 {
		return "", errors.New("candidate config requires version 1 and at most 64 selections")
	}
	seen := map[string]bool{}
	for _, s := range c.Selections {
		if (s.Type != "connection" && s.Type != "mcp_server") || !candidateName.MatchString(s.Slug) || (s.ResourceID == "") == (s.FixtureName == "") || seen[s.Type+"/"+s.Slug] {
			return "", errors.New("candidate selection requires a unique known type/slot and exactly one resource or fixture")
		}
		seen[s.Type+"/"+s.Slug] = true
		if s.FixtureName != "" && (s.Type != "connection" || !candidateName.MatchString(s.FixtureName)) {
			return "", errors.New("candidate fixtures require a named HTTP connection fixture")
		}
		if s.ResourceID != "" {
			id, err := uuid.Parse(s.ResourceID)
			if err != nil || id == uuid.Nil || id.String() != s.ResourceID {
				return "", errors.New("candidate resource ID must be canonical non-nil UUID")
			}
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	if len(raw) > 16<<10 {
		return "", errors.New("candidate config header exceeds 16 KiB")
	}
	return string(raw), nil
}
