//go:build windows

package connector

import (
	"context"
	"testing"
)

func TestWindowsIdentityValidationRejectsOrdinaryProcess(t *testing.T) {
	err := runServiceIdentityValidation(context.Background(), `NT SERVICE\airlock-validation`, "airlock-validation", `C:\ProgramData\Airlock\Connectors\sample\.service-validation-test.json`, func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("service identity validation ran outside the Service Control Manager")
	}
}
