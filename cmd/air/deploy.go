package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/scaffold"
)

var (
	deployUUIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	agentSlugRe  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

type deployFlags struct {
	create      bool
	slug        string
	agent       string
	url         string
	name        string
	description string
	dir         string
}

func cmdDeploy(args []string) error {
	f, err := parseDeployFlags(args)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(f.dir, "go.mod")); err != nil {
		return fmt.Errorf("deploy requires an agent repo with go.mod in %s: %w", f.dir, err)
	}
	binding, _, err := loadAgentBinding(f.dir)
	if err != nil {
		return err
	}
	baseURL := normalizeBaseURL(f.url)
	if baseURL == "" {
		baseURL = binding.AirlockURL
	}
	if baseURL == "" {
		return errors.New("deploy needs an Airlock URL: pass --url or run air init --airlock <url>")
	}

	ctx := context.Background()
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		return err
	}

	before, err := snapshotManagedFiles(f.dir)
	if err != nil {
		return err
	}
	if err := runUpdate(f.dir, scaffold.ScaffoldData{
		AgentSDKVersion: "v" + agentsdk.Version,
		AgentBaseImage:  defaultBaseImage,
	}); err != nil {
		return err
	}
	after, err := snapshotManagedFiles(f.dir)
	if err != nil {
		return err
	}
	if changed := changedManagedFiles(before, after); len(changed) > 0 {
		return fmt.Errorf("update changed airlock-managed files (%s); review the changes and rerun deploy", strings.Join(changed, ", "))
	}

	var target agentBinding
	if f.create {
		target, err = createDraftAgent(ctx, baseURL, token, f)
		if err != nil {
			return err
		}
		binding.AirlockURL = baseURL
		binding.AgentID = target.AgentID
		binding.Slug = target.Slug
		if err := writeAgentBinding(f.dir, binding); err != nil {
			return err
		}
	} else {
		target, err = resolveDeployTarget(ctx, baseURL, token, f.agent, binding)
		if err != nil {
			return err
		}
	}

	fmt.Printf("Deploying %s to %s (%s) at %s\n", f.dir, target.Slug, target.AgentID, baseURL)
	if err := uploadSource(ctx, baseURL, token, target.AgentID, f.dir); err != nil {
		return err
	}
	binding.AirlockURL = baseURL
	binding.AgentID = target.AgentID
	binding.Slug = target.Slug
	if err := writeAgentBinding(f.dir, binding); err != nil {
		return err
	}
	fmt.Println("Source uploaded; build started")
	return nil
}

func parseDeployFlags(args []string) (deployFlags, error) {
	f := deployFlags{dir: "."}
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[:2] != "--" {
			positional = append(positional, a)
			continue
		}
		key := a[2:]
		if key == "create" {
			f.create = true
			continue
		}
		if i+1 >= len(args) {
			return deployFlags{}, fmt.Errorf("flag --%s needs a value", key)
		}
		i++
		value := args[i]
		switch key {
		case "slug":
			f.slug = value
		case "agent":
			f.agent = value
		case "url":
			f.url = value
		case "name":
			f.name = value
		case "description":
			f.description = value
		default:
			return deployFlags{}, fmt.Errorf("unknown flag --%s", key)
		}
	}
	switch len(positional) {
	case 0:
	case 1:
		f.dir = positional[0]
	default:
		return deployFlags{}, errors.New("deploy takes at most one positional argument: the agent repo directory")
	}
	if f.create && f.agent != "" {
		return deployFlags{}, errors.New("deploy cannot combine --create and --agent")
	}
	if f.create {
		if f.name == "" {
			if f.slug != "" {
				f.name = f.slug
			} else {
				f.name = filepath.Base(filepath.Clean(f.dir))
			}
		}
		if f.slug == "" {
			f.slug = slugFromName(f.name)
			if f.slug == "" {
				return deployFlags{}, fmt.Errorf("could not derive a slug from name %q; pass --slug", f.name)
			}
		}
		if !validAgentSlug(f.slug) {
			return deployFlags{}, fmt.Errorf("invalid slug %q: use 2-63 lowercase letters, digits, and single dashes", f.slug)
		}
	}
	return f, nil
}

func slugFromName(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 63 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) < 2 || !validAgentSlug(out) {
		return ""
	}
	return out
}

func validAgentSlug(s string) bool {
	if len(s) < 2 || len(s) > 63 {
		return false
	}
	return agentSlugRe.MatchString(s)
}

func createDraftAgent(ctx context.Context, baseURL, token string, f deployFlags) (agentBinding, error) {
	name := f.name
	if name == "" {
		name = f.slug
	}
	var resp airlockv1.CreateAgentResponse
	err := doProto(ctx, baseURL, http.MethodPost, "/api/v1/agents", token, &airlockv1.CreateAgentRequest{
		Name:             name,
		Slug:             f.slug,
		Description:      f.description,
		SkipInitialBuild: true,
	}, &resp)
	if err != nil {
		return agentBinding{}, fmt.Errorf("create agent: %w", err)
	}
	if resp.Agent == nil || resp.Agent.Id == "" {
		return agentBinding{}, errors.New("create agent response did not include an agent id")
	}
	return agentBinding{AirlockURL: baseURL, AgentID: resp.Agent.Id, Slug: resp.Agent.Slug}, nil
}

func resolveDeployTarget(ctx context.Context, baseURL, token, flagAgent string, binding agentBinding) (agentBinding, error) {
	target := flagAgent
	if target == "" {
		target = binding.AgentID
	}
	if target == "" && binding.Slug != "" {
		return agentBinding{}, fmt.Errorf("%s has slug %q but no agent_id; run air deploy --agent %s once to resolve and persist the stable binding", agentBindingPath, binding.Slug, binding.Slug)
	}
	if target == "" {
		return agentBinding{}, errors.New("deploy needs a target: pass --agent, --create, or set .airlock/agent.toml")
	}
	if deployUUIDRe.MatchString(target) {
		agent, err := getAgentDetail(ctx, baseURL, token, target)
		if err != nil {
			return agentBinding{}, err
		}
		if binding.AgentID == target && binding.Slug != "" && agent.GetSlug() != binding.Slug {
			return agentBinding{}, fmt.Errorf("%s has agent_id %s but slug %q; Airlock reports slug %q", agentBindingPath, target, binding.Slug, agent.GetSlug())
		}
		return agentBinding{AirlockURL: baseURL, AgentID: target, Slug: agent.GetSlug()}, nil
	}

	var resp airlockv1.ListAgentsResponse
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agents", token, nil, &resp); err != nil {
		return agentBinding{}, fmt.Errorf("resolve agent %q: %w", target, err)
	}
	for _, a := range resp.Agents {
		if a.GetSlug() == target || a.GetId() == target {
			if binding.AgentID != "" && binding.Slug == target && a.GetId() != binding.AgentID {
				return agentBinding{}, fmt.Errorf("%s has slug %q but agent_id %s; Airlock reports agent_id %s", agentBindingPath, binding.Slug, binding.AgentID, a.GetId())
			}
			return agentBinding{AirlockURL: baseURL, AgentID: a.GetId(), Slug: a.GetSlug()}, nil
		}
	}
	return agentBinding{}, fmt.Errorf("agent %q not found in %s", target, baseURL)
}

func getAgentDetail(ctx context.Context, baseURL, token, agentID string) (*airlockv1.AgentInfo, error) {
	var resp airlockv1.GetAgentDetailResponse
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agents/"+agentID, token, nil, &resp); err != nil {
		return nil, fmt.Errorf("resolve agent %q: %w", agentID, err)
	}
	if resp.Agent == nil || resp.Agent.Id == "" {
		return nil, fmt.Errorf("resolve agent %q: response did not include agent detail", agentID)
	}
	return resp.Agent, nil
}

func uploadSource(ctx context.Context, baseURL, token, agentID, dir string) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeSourceArchive(pw, dir))
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, normalizeBaseURL(baseURL)+"/api/v1/agents/"+agentID+"/source", pr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := apiClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		var er airlockv1.ErrorResponse
		if err := protoUnmarshal.Unmarshal(b, &er); err == nil && er.Error != "" {
			return fmt.Errorf("upload source: %s: %s", resp.Status, er.Error)
		}
		return fmt.Errorf("upload source: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

func writeSourceArchive(w io.Writer, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("source archive requires go.mod at repo root: %w", err)
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if skipArchivePath(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeType != 0 {
			if d.IsDir() {
				return nil
			}
			return fmt.Errorf("source archive does not support special file %s", rel)
		}
		if d.IsDir() {
			return nil
		}
		h := &tar.Header{Name: rel, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime().UTC().Truncate(time.Second)}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	return err
}

func skipArchivePath(rel string, isDir bool) bool {
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return true
	}
	if rel == ".airlock/local" || strings.HasPrefix(rel, ".airlock/local/") {
		return true
	}
	if rel == ".airlock/toolchain" || strings.HasPrefix(rel, ".airlock/toolchain/") {
		return true
	}
	if isDir {
		switch rel {
		case "node_modules", ".cache", ".tmp":
			return true
		}
	}
	return false
}

func snapshotManagedFiles(dir string) (map[string][]byte, error) {
	files := []string{"Dockerfile", "AGENTS.md", scaffold.NoticesFilename}
	out := make(map[string][]byte, len(files))
	for _, file := range files {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		out[file] = b
	}
	return out, nil
}

func changedManagedFiles(before, after map[string][]byte) []string {
	var changed []string
	for name, b := range before {
		if string(b) != string(after[name]) {
			changed = append(changed, name)
		}
	}
	return changed
}
