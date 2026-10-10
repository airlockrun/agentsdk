package localruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/sol/session"
)

type taskStore struct{ controller *taskController }

func (s *taskStore) Load(ctx context.Context) (messages []session.Message, err error) {
	err = s.controller.update(ctx, func(_ *stateData, t *taskRecord) error { messages = clone(t.Messages); return nil })
	return
}
func (*taskStore) Append(context.Context, []session.Message) error {
	return errors.New("localruntime: task transcript requires atomic checkpoint publication")
}
func (s *taskStore) Compact(ctx context.Context, messages []session.Message, _ int) error {
	return s.controller.update(ctx, func(_ *stateData, t *taskRecord) error { t.Messages = clone(messages); return nil })
}
func (s *taskStore) LoadCheckpoint(ctx context.Context) (cp *agentruntime.Checkpoint, err error) {
	err = s.controller.update(ctx, func(_ *stateData, t *taskRecord) error {
		cp = clone(t.Checkpoint)
		if cp != nil {
			cp.Append = nil
		}
		return nil
	})
	return
}
func (s *taskStore) SaveCheckpoint(ctx context.Context, cp *agentruntime.Checkpoint) error {
	if cp == nil {
		return errors.New("localruntime: checkpoint required")
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	return s.controller.update(ctx, func(_ *stateData, t *taskRecord) error {
		expected := int64(1)
		if t.Checkpoint != nil {
			if cp.Revision == t.Checkpoint.Revision && bytes.Equal(raw, t.CheckpointPayload) {
				return nil
			}
			expected = t.Checkpoint.Revision + 1
		}
		if cp.Revision != expected {
			return errors.New("localruntime: stale checkpoint revision")
		}
		t.Messages = append(t.Messages, clone(cp.Append)...)
		t.Checkpoint = clone(cp)
		t.Checkpoint.Append = nil
		t.CheckpointPayload = raw
		return nil
	})
}

func (c *taskController) update(ctx context.Context, fn func(*stateData, *taskRecord) error) error {
	return c.host.config.State.update(ctx, func(d *stateData) error {
		t := d.Tasks[c.id]
		if t == nil || t.Owner != c.owner || t.Info.Status != "running" || !t.LeaseUntil.After(time.Now()) {
			return errors.New("localruntime: task owner fence is not live")
		}
		return fn(d, t)
	})
}
