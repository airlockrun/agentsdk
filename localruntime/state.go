package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/sol/session"
	"github.com/gofrs/flock"
)

type taskRecord struct {
	Info              wire.AgentRunInfo
	Input             string
	RequestID         string
	ParentID          string
	Scope             wire.RuntimeContext
	LeaseUntil        time.Time
	Owner             string
	Attempts          int32
	Messages          []session.Message
	Checkpoint        *agentruntime.Checkpoint
	CheckpointPayload json.RawMessage
	WaitID            string
	WaitRequest       wire.AgentWaitRequest
	WaitDeadline      *time.Time
}

type runRecord struct {
	Scope           wire.RuntimeContext
	Closed          bool
	Completion      json.RawMessage
	ExpiresAt       time.Time
	ResourceSession string
}

type jobRecord struct {
	Info            wire.JobInfo
	Caller          wire.Caller
	Owner           string
	LeaseUntil      time.Time
	RunID           string
	ResourceSession string
}

type stateData struct {
	AppID    string
	Manifest wire.AgentManifest
	Tasks    map[string]*taskRecord
	Runs     map[string]*runRecord
	Jobs     map[string]*jobRecord
	Crons    map[string]time.Time
}

// State serializes durable local control state on a shared filesystem. The lock
// spans read, mutation, fsync and atomic publication, never network effects.
type State struct{ directory string }

func OpenState(directory, appID string) (*State, error) {
	if directory == "" || appID == "" {
		return nil, errors.New("localruntime: state directory and app ID are required")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	s := &State{directory: directory}
	err := s.update(context.Background(), func(d *stateData) error {
		if d.AppID != "" && d.AppID != appID {
			return errors.New("localruntime: persisted app identity mismatch")
		}
		d.AppID = appID
		return nil
	})
	return s, err
}

func (s *State) update(ctx context.Context, fn func(*stateData) error) error {
	lock := flock.New(filepath.Join(s.directory, "state.lock"))
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	defer lock.Unlock()
	d := stateData{Tasks: map[string]*taskRecord{}, Runs: map[string]*runRecord{}, Jobs: map[string]*jobRecord{}, Crons: map[string]time.Time{}}
	name := filepath.Join(s.directory, "state.json")
	raw, err := os.ReadFile(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
	}
	if d.Tasks == nil || d.Runs == nil || d.Jobs == nil {
		return errors.New("localruntime: invalid persisted state")
	}
	if err := fn(&d); err != nil {
		return err
	}
	raw, err = json.Marshal(d)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.directory, ".state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(raw)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), name); err != nil {
		return err
	}
	dir, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func clone[T any](v T) T {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}
