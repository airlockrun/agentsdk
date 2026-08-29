//go:build windows

package connector

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows/svc"
)

func runAsService(ctx context.Context, name string, run func(context.Context) error) error {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return fmt.Errorf("connector: identify Windows service process: %w", err)
	}
	if !isService {
		return run(ctx)
	}
	return svc.Run(name, &windowsServiceHandler{parent: ctx, run: run})
}

type windowsServiceHandler struct {
	parent context.Context
	run    func(context.Context) error
}

func (h *windowsServiceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(h.parent)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.run(ctx) }()
	status <- svc.Status{State: svc.StartPending}
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	var stopTimer <-chan time.Time
	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				if stopTimer == nil {
					timer := time.NewTimer(windowsServiceWaitTimeout)
					defer timer.Stop()
					stopTimer = timer.C
				}
			case svc.Interrogate:
				status <- request.CurrentStatus
			}
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				return true, 1
			}
			return false, 0
		case <-stopTimer:
			return true, 1
		}
	}
}
