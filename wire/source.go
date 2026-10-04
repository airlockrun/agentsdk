package wire

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	SourceStateHeader      = "X-Airlock-Source-State"
	SourceRevisionHeader   = "X-Airlock-Source-Revision"
	SourceGenerationHeader = "X-Airlock-Source-Generation"
	SourceTaskHeader       = "X-Airlock-Task-ID"

	sourceETagPrefix = "airlock-source-v1:"
)

// SourceVersion is the canonical compare coordinate for application source.
// Revision identifies exact Git history; Generation changes only on deployment.
type SourceVersion struct {
	Revision   string
	Generation int64
}

func (v SourceVersion) ETag() string {
	return sourceETagPrefix + v.Revision + ":" + strconv.FormatInt(v.Generation, 10)
}

func ParseSourceETag(value string) (SourceVersion, error) {
	value = strings.TrimSpace(value)
	if unquoted, err := strconv.Unquote(value); err == nil {
		value = unquoted
	}
	if !strings.HasPrefix(value, sourceETagPrefix) {
		return SourceVersion{}, errors.New("source ETag uses an unsupported protocol; update the air CLI")
	}
	revision, generationText, ok := strings.Cut(strings.TrimPrefix(value, sourceETagPrefix), ":")
	if !ok || revision == "" {
		return SourceVersion{}, errors.New("invalid source ETag")
	}
	generation, err := strconv.ParseInt(generationText, 10, 64)
	if err != nil || generation < 0 {
		return SourceVersion{}, fmt.Errorf("invalid source ETag generation %q", generationText)
	}
	return SourceVersion{Revision: revision, Generation: generation}, nil
}
