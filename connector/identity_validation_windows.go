//go:build windows

package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func runServiceIdentityValidation(ctx context.Context, identity, serviceName, resultPath string, validate func(context.Context) error) error {
	if !strings.EqualFold(identity, `NT SERVICE\`+serviceName) || serviceName == "" || resultPath == "" {
		return errors.New("connector: Windows service validation requires its virtual service account and temporary service metadata")
	}
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("connector: identify Windows service process: %w", err)
	}
	if !isService {
		return errors.New("connector: Windows service validation must run under the Service Control Manager")
	}
	return svc.Run(serviceName, &windowsServiceHandler{parent: ctx, run: func(runCtx context.Context) error {
		tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return err
		}
		expectedSID, _, _, err := windows.LookupSID("", identity)
		if err != nil {
			return fmt.Errorf("connector: resolve service identity %s: %w", identity, err)
		}
		validation := serviceValidationResult{}
		if tokenUser.User.Sid.String() != expectedSID.String() {
			validation.Error = fmt.Sprintf("connector: service validation is running as SID %s, want %s", tokenUser.User.Sid.String(), expectedSID.String())
		} else if err := validate(runCtx); err != nil {
			validation.Error = err.Error()
		}
		body, err := json.Marshal(validation)
		if err != nil {
			return err
		}
		return atomicWrite(resultPath, body, 0o600)
	}})
}
