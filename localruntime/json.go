// Package localruntime implements app-runtime v3 adapters for local execution.
// Providers, identities and persistence are explicit dependencies.
package localruntime

import (
	"encoding/json"
	"errors"
	"io"
)

func strictJSON(reader io.Reader, destination any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
