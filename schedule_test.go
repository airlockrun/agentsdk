package agentsdk

import (
	"context"
	"testing"
	"time"
)

func noopFire(context.Context, *EventWriter) error { return nil }

func TestRegisterScheduleSlug_UniqueAcrossKinds(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterCron(&Cron{Slug: "x", Schedule: "0 9 * * *", Handler: noopFire})
	defer func() {
		if recover() == nil {
			t.Fatal("registering a schedule with a taken cron slug should panic")
		}
	}()
	a.RegisterSchedule(&Schedule{Slug: "x", Handler: noopFire})
}

func TestScheduleAt_UnknownSlug(t *testing.T) {
	a, _ := testAgent(t)
	if _, err := a.ScheduleAt(context.Background(), ScheduleAtRequest{Slug: "nope", FireAt: time.Unix(1, 0)}); err == nil {
		t.Fatal("ScheduleAt with an unregistered slug should error")
	}
}

func TestScheduleFromContext(t *testing.T) {
	a, _ := testAgent(t)

	if _, ok := ScheduleFromContext(context.Background()); ok {
		t.Error("ScheduleFromContext(background) should be absent")
	}

	r := newRun(a, "run-1", "", "", context.Background())
	r.fireID = "fire-123"
	r.fireSlug = "remind"
	ref, ok := ScheduleFromContext(contextWithRun(context.Background(), r))
	if !ok || ref.FireID != "fire-123" || ref.Slug != "remind" {
		t.Errorf("ScheduleFromContext = %+v, ok=%v", ref, ok)
	}
}
