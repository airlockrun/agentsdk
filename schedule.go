package agentsdk

import (
	"context"
	"fmt"
	"net/url"
	"time"
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

// ScheduleAt arms a one-shot fire of a registered handler at fireAt and returns
// the fire id. Store that id with your per-instance data in the agent's own DB;
// the fire handler recovers it via ScheduleFromContext. The slug must name a
// registered cron or schedule.
func (a *Agent) ScheduleAt(ctx context.Context, req ScheduleAtRequest) (string, error) {
	if _, ok := a.scheduleHandlers[req.Slug]; !ok {
		return "", fmt.Errorf("agentsdk: ScheduleAt: no registered handler %q", req.Slug)
	}
	if req.FireAt.IsZero() {
		return "", fmt.Errorf("agentsdk: ScheduleAt(%q): FireAt is required", req.Slug)
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := a.client.doJSON(ctx, "POST", "/api/agent/schedules", req, &resp); err != nil {
		return "", err
	}
	return resp.ID, nil
}

// CancelSchedule removes a pending fire by id. It is a no-op if the fire already
// fired or never existed.
func (a *Agent) CancelSchedule(ctx context.Context, id string) error {
	return a.client.doJSON(ctx, "DELETE", "/api/agent/schedules/"+url.PathEscape(id), nil, nil)
}

// ListSchedulesFilter narrows ListSchedules. An empty Slug lists every pending
// fire for the agent.
type ListSchedulesFilter struct {
	Slug string
}

// ListSchedules returns the agent's pending fires, optionally for one slug.
func (a *Agent) ListSchedules(ctx context.Context, f ListSchedulesFilter) ([]ScheduledFire, error) {
	path := "/api/agent/schedules"
	if f.Slug != "" {
		path += "?slug=" + url.QueryEscape(f.Slug)
	}
	var resp struct {
		Fires []ScheduledFire `json:"fires"`
	}
	if err := a.client.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Fires, nil
}

// ScheduleFromContext returns the fire that triggered the current handler run.
// The second return is false outside a /fire handler (a normal prompt/webhook
// run). Use FireID to look up the per-instance data stored at ScheduleAt time.
func ScheduleFromContext(ctx context.Context) (ScheduledFireRef, bool) {
	if r := runFromContext(ctx); r != nil && r.fireSlug != "" {
		return ScheduledFireRef{FireID: r.fireID, Slug: r.fireSlug}, true
	}
	return ScheduledFireRef{}, false
}
