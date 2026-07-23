package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
)

func noopFire(context.Context, ScheduleEvent) error { return nil }

func TestRegisterScheduleSlug_UniqueAcrossKinds(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterCron(&Cron{Slug: "x", Schedule: "0 9 * * *", Handler: noopFire, Description: "Daily task"})
	defer func() {
		if recover() == nil {
			t.Fatal("registering a schedule with a taken cron slug should panic")
		}
	}()
	a.RegisterSchedule(&Schedule{Slug: "x", Handler: noopFire, Description: "One-shot task"})
}

func TestScheduleAt(t *testing.T) {
	a, mock := testAgent(t)
	a.RegisterSchedule(&Schedule{Slug: "remind", Handler: noopFire, Description: "One-shot task"})
	fireAt := time.Date(2026, 7, 23, 12, 0, 0, 123456789, time.FixedZone("offset", 3600))
	req := ScheduleRequest{ID: "11111111-1111-1111-1111-111111111111", Slug: "remind", FireAt: fireAt}
	if err := a.ScheduleAt(context.Background(), req); err != nil {
		t.Fatalf("ScheduleAt: %v", err)
	}
	requests := mock.RequestsByPath("/api/agent/schedules")
	if len(requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(requests))
	}
	var got wire.ScheduleRequest
	if err := json.Unmarshal(requests[0].Body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != req.ID || got.Slug != req.Slug || !got.FireAt.Equal(fireAt.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("request = %+v", got)
	}
}

func TestScheduleAtRejectsInvalidIDAndCron(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterSchedule(&Schedule{Slug: "remind", Handler: noopFire, Description: "One-shot task"})
	a.RegisterCron(&Cron{Slug: "daily", Schedule: "0 9 * * *", Handler: noopFire, Description: "Daily task"})
	if err := a.ScheduleAt(context.Background(), ScheduleRequest{ID: "not-a-uuid", Slug: "remind", FireAt: time.Now()}); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if err := a.ScheduleAt(context.Background(), ScheduleRequest{ID: "11111111-1111-1111-1111-111111111111", Slug: "daily", FireAt: time.Now()}); err == nil {
		t.Fatal("cron accepted as one-shot")
	}
}

func TestHandleFireReturnsTypedResult(t *testing.T) {
	a, _ := testAgent(t)
	var received ScheduleEvent
	a.RegisterSchedule(&Schedule{
		Slug: "remind", Description: "One-shot task",
		Handler: func(ctx context.Context, event ScheduleEvent) error {
			received = event
			return errors.New("retry me")
		},
	})
	body := `{"id":"11111111-1111-1111-1111-111111111111","slug":"remind","scheduledAt":"2026-07-23T12:00:00Z","attempt":2}`
	req := httptest.NewRequest("POST", "/fire/remind", strings.NewReader(body))
	req.SetPathValue("slug", "remind")
	req.Header.Set("X-Run-ID", "22222222-2222-2222-2222-222222222222")
	w := httptest.NewRecorder()
	a.handleFire(w, req)
	var result wire.ScheduleFireResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "error" || result.Error != "retry me" || received.ID == "" || received.Attempt != 2 {
		t.Fatalf("result=%+v event=%+v", result, received)
	}
}

func TestScheduleAt_UnknownSlug(t *testing.T) {
	a, _ := testAgent(t)
	if err := a.ScheduleAt(context.Background(), ScheduleRequest{ID: "11111111-1111-1111-1111-111111111111", Slug: "nope", FireAt: time.Unix(1, 0)}); err == nil {
		t.Fatal("ScheduleAt with an unregistered slug should error")
	}
}
