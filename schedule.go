package agentsdk

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

// scheduleHandler is a registered cron or schedule handler. The slug is unique
// per agent across both kinds; POST /fire/{slug} dispatches to it.
type scheduleHandler struct {
	slug        string
	kind        string // "cron" | "schedule"
	recurrence  string // cron expression (kind=="cron"); empty for schedules
	handler     ScheduleHandlerFunc
	timeout     time.Duration
	description string
}

// ScheduleAt idempotently arms a caller-identified one-shot occurrence. Store
// domain data under ID before calling this method; retry the identical request
// after uncertainty.
func (a *Agent) ScheduleAt(ctx context.Context, req ScheduleRequest) error {
	handler, ok := a.scheduleHandlers[req.Slug]
	if !ok {
		return fmt.Errorf("agentsdk: ScheduleAt: no registered handler %q", req.Slug)
	}
	if handler.kind != "schedule" {
		return fmt.Errorf("agentsdk: ScheduleAt(%q): handler is not a one-shot schedule", req.Slug)
	}
	id, err := uuid.Parse(req.ID)
	if err != nil || id == uuid.Nil || id.String() != req.ID {
		return fmt.Errorf("agentsdk: ScheduleAt(%q): ID must be a canonical non-nil UUID", req.Slug)
	}
	if req.FireAt.IsZero() {
		return fmt.Errorf("agentsdk: ScheduleAt(%q): FireAt is required", req.Slug)
	}
	body := wire.ScheduleRequest{ID: req.ID, Slug: req.Slug, FireAt: req.FireAt.UTC().Truncate(time.Microsecond)}
	return a.client.doJSON(ctx, "POST", "/api/agent/schedules", body, nil)
}

// CancelSchedule removes a pending fire by id. It is a no-op if the fire already
// fired or never existed.
func (a *Agent) CancelSchedule(ctx context.Context, id string) error {
	return a.client.doJSON(ctx, "DELETE", "/api/agent/schedules/"+url.PathEscape(id), nil, nil)
}

// ListSchedulesFilter narrows ListSchedules. An empty Slug lists every pending
// fire for the agent.
type ListSchedulesFilter struct {
	noUnkeyedLiterals

	Slug string
}

// ListSchedules returns the agent's pending fires, optionally for one slug.
func (a *Agent) ListSchedules(ctx context.Context, f ListSchedulesFilter) ([]ScheduledFire, error) {
	path := "/api/agent/schedules"
	if f.Slug != "" {
		path += "?slug=" + url.QueryEscape(f.Slug)
	}
	var resp struct {
		Fires []wire.ScheduledFire `json:"fires"`
	}
	if err := a.client.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	fires := make([]ScheduledFire, len(resp.Fires))
	for i, fire := range resp.Fires {
		fires[i] = ScheduledFire{
			ID: fire.ID, Slug: fire.Slug, Kind: ScheduleKind(fire.Kind), FireAt: fire.FireAt,
			Status: ScheduleStatus(fire.Status), Recurrence: fire.Recurrence,
		}
	}
	return fires, nil
}
