package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

func lockPreviewSelection(ctx context.Context, dir string) (func(), error) {
	parent := filepath.Join(dir, ".airlock", "local")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(parent, "preview-selection.lock"))
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	locked, err := lock.TryLockContext(bounded, 20*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("preview selection is busy")
	}
	return func() { _ = lock.Unlock() }, nil
}

func (s *selectedDeployment) persistRemote(savePreview bool) error {
	latest, _, err := loadAgentBinding(s.flags.dir)
	if err != nil {
		return err
	}
	s.binding = latest
	bound, ok := s.binding.remote(s.remoteName)
	if !ok {
		bound = agentRemoteBinding{AirlockURL: s.baseURL}
	}
	if bound.AgentID == "" {
		bound.AgentID, bound.Slug = s.app.AgentID, s.app.Slug
	}
	bound.AirlockURL = s.baseURL
	if ref, ok := s.app.Previews[s.app.AgentID]; savePreview && ok {
		if bound.Previews == nil {
			bound.Previews = map[string]previewReference{}
		}
		bound.Previews[s.app.AgentID] = ref
	}
	if s.info != nil {
		if state, ok := s.app.Targets[s.info.Id]; ok {
			if bound.Targets == nil {
				bound.Targets = map[string]targetSourceBinding{}
			}
			bound.Targets[s.info.Id] = state
		}
	}
	s.binding.putRemote(s.remoteName, bound)
	return writeAgentBinding(s.flags.dir, s.binding)
}

func (s *selectedDeployment) savePreviewReference() error {
	if s.info == nil || s.info.Kind != "personal_preview" {
		return errors.New("resolved preview is required")
	}
	if s.app.Previews == nil {
		s.app.Previews = map[string]previewReference{}
	}
	s.app.Previews[s.app.AgentID] = previewReference{DeploymentID: s.info.Id, Name: s.info.Name}
	return s.persistRemote(true)
}

func cmdPreviewSelect(args []string) error {
	f, pos, err := parseSelectionFlags(args)
	if err != nil {
		return err
	}
	if f.target != "" || len(pos) != 1 || pos[0] == "preview" || pos[0] == "production" || !validRemoteName(pos[0]) {
		return errors.New("preview select requires a concrete preview name/UUID, without --target")
	}
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		return errors.New("hosted preview selection is injected by Airlock")
	}
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	release, err := lockPreviewSelection(ctx, f.dir)
	if err != nil {
		return err
	}
	defer release()
	f.target = pos[0]
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	if err := s.savePreviewReference(); err != nil {
		return err
	}
	s.flags.target = "preview"
	return s.print()
}
