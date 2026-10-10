package localruntime

import (
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

func (h *Host) scheduleCrons(d *stateData) error {
	now := time.Now().UTC()
	for _, c := range d.Manifest.JobCrons {
		next := d.Crons[c.Slug]
		if next.IsZero() || next.After(now) {
			continue
		}
		schedule, err := cron.ParseStandard(c.Schedule)
		if err != nil {
			return err
		}
		def, err := jobDefinition(d, c.HandlerName, c.HandlerVersion, c.InputSchemaHash, c.OutputSchemaHash)
		if err != nil {
			return err
		}
		id := uuid.NewSHA1(uuid.MustParse(h.config.AppID), []byte(c.Slug+":"+next.UTC().Format(time.RFC3339Nano))).String()
		if d.Jobs[id] == nil {
			caller := wire.Caller{Kind: "application", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "schedule", Execution: "job"}}
			info := wire.JobInfo{ID: id, AgentID: h.config.AppID, HandlerName: def.Name, HandlerVersion: def.Version, InputSchemaHash: def.InputSchemaHash, OutputSchemaHash: def.OutputSchemaHash, Input: c.Input, Status: "queued", MaxAttempts: def.MaxAttempts, AttemptLimit: def.MaxAttempts, ScheduledAt: &next, CreatedAt: now, UpdatedAt: now}
			d.Jobs[id] = &jobRecord{Info: info, Caller: caller}
		}
		d.Crons[c.Slug] = schedule.Next(next)
	}
	return nil
}
