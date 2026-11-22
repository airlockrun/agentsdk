package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckerScriptWithInheritedGitCoordinates(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	vars, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		t.Fatal(err)
	}
	local := map[string]bool{}
	for _, key := range strings.Fields(string(vars)) {
		local[key] = true
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !local[key] && !strings.HasPrefix(key, "GIT_CONFIG_KEY_") && !strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			env = append(env, entry)
		}
	}
	git := func(dir string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir, cmd.Env = dir, env
		data, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, data)
		}
		return data
	}
	fixture := filepath.Join(t.TempDir(), "sdk")
	git(root, "clone", "--shared", root, fixture)
	// The fixture has an exact stable API baseline, independent of later tags.
	for _, tag := range strings.Fields(string(git(fixture, "tag", "--list"))) {
		if tag != "v0.9.0" {
			git(fixture, "tag", "-d", tag)
		}
	}
	for _, name := range bytes.Split(bytes.TrimRight(git(root, "ls-files", "-z"), "\x00"), []byte{0}) {
		path := string(name)
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(fixture, path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			t.Fatal(err)
		}
	}
	foreign := t.TempDir()
	git(foreign, "init", "-b", "main")
	git(foreign, "config", "user.name", "Foreign Fixture")
	git(foreign, "config", "user.email", "foreign@example.invalid")
	git(foreign, "-c", "core.hooksPath=/dev/null", "commit", "--allow-empty", "-m", "foreign")
	gitDir := filepath.Join(foreign, ".git")
	// Materialize an index to verify that the caller's staged state is untouched.
	git(foreign, "read-tree", "HEAD")
	before := map[string][]byte{}
	for _, name := range []string{"HEAD", "index", "config", "refs/heads/main"} {
		data, err := os.ReadFile(filepath.Join(gitDir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	for _, tc := range []struct {
		name, version string
		wantFailure   bool
	}{
		{"approved new series", "0.10.0", false},
		{"stable API violation", "0.9.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(fixture, "agentsdk.go")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(data), "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "const Version = ") {
					lines[i] = "const Version = \"" + tc.version + "\""
				}
			}
			if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", filepath.Join(fixture, "scripts/check-api-compat.sh"))
			cmd.Dir = filepath.Join(fixture, "internal/apicompat")
			cmd.Env = append(append([]string{}, env...), "GIT_DIR="+gitDir, "GIT_WORK_TREE="+foreign,
				"GIT_COMMON_DIR="+gitDir, "GIT_INDEX_FILE="+filepath.Join(gitDir, "index"), "GIT_PREFIX=foreign/")
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantFailure {
				t.Fatalf("checker error = %v, output: %s", err, output)
			}
			if !strings.Contains(string(output), "Options.Model: removed") {
				t.Fatalf("real incompatible API diagnostic missing: %s", output)
			}
			if tc.wantFailure && !strings.Contains(string(output), "incompatible API change(s) in compatibility series v0.9") {
				t.Fatalf("stable API violation not rejected: %s", output)
			}
			for name, want := range before {
				got, err := os.ReadFile(filepath.Join(gitDir, name))
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("foreign Git %s changed: %v", name, err)
				}
			}
		})
	}
}

func TestSelectBaseline(t *testing.T) {
	tests := []struct {
		name          string
		tags          []string
		current       string
		currentTagged bool
		wantBaseline  string
		wantBreaking  bool
		wantErr       bool
	}{
		{
			name:         "pre-1.0 prerelease allows breaking change",
			tags:         []string{"v0.3.0", "v0.4.0-rc.2", "v0.4.0-rc.10"},
			current:      "v0.4.0-rc.11",
			wantBaseline: "v0.4.0-rc.10",
			wantBreaking: true,
		},
		{
			name:         "first release in new zero-major series",
			tags:         []string{"v0.3.0", "v0.4.9"},
			current:      "v0.5.0-rc.1",
			wantBaseline: "v0.4.9",
			wantBreaking: true,
		},
		{
			name:         "first stable release after prereleases",
			tags:         []string{"v0.6.2", "v0.7.0-rc.4", "v0.7.0-rc.5"},
			current:      "v0.7.0",
			wantBaseline: "v0.7.0-rc.5",
			wantBreaking: true,
		},
		{
			name:         "prerelease preserves stable zero-major series",
			tags:         []string{"v0.3.0", "v0.4.0"},
			current:      "v0.4.1-rc.1",
			wantBaseline: "v0.4.0",
		},
		{
			name:         "latest stable major series",
			tags:         []string{"v1.8.0", "v2.0.0-rc.1", "v2.0.0"},
			current:      "v2.1.0",
			wantBaseline: "v2.0.0",
		},
		{
			name:          "tagged release compares with previous tag",
			tags:          []string{"v0.4.0-rc.31", "v0.4.0-rc.32"},
			current:       "v0.4.0-rc.32",
			currentTagged: true,
			wantBaseline:  "v0.4.0-rc.31",
			wantBreaking:  true,
		},
		{
			name:    "untagged commit rejects published version",
			tags:    []string{"v0.4.0-rc.31", "v0.4.0-rc.32"},
			current: "v0.4.0-rc.32",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline, breaking, err := selectBaseline(tt.tags, tt.current, tt.currentTagged)
			if tt.wantErr {
				if err == nil {
					t.Fatal("selectBaseline succeeded, want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if baseline != tt.wantBaseline || breaking != tt.wantBreaking {
				t.Fatalf("got (%q, %v), want (%q, %v)", baseline, breaking, tt.wantBaseline, tt.wantBreaking)
			}
		})
	}
}

func TestIgnoredMetadataChange(t *testing.T) {
	if !ignoredMetadataChange("github.com/airlockrun/agentsdk", "Version: value changed from 1 to 2") {
		t.Fatal("Version value changes must be ignored")
	}
	if ignoredMetadataChange("github.com/airlockrun/agentsdk", "Version: changed from untyped string to string") {
		t.Fatal("Version type changes must not be ignored")
	}
	if ignoredMetadataChange("github.com/airlockrun/agentsdk/agenttest", "Version: value changed from 1 to 2") {
		t.Fatal("metadata exceptions must be package-specific")
	}
	if !ignoredMetadataChange("github.com/airlockrun/agentsdk/lucide", "Version: value changed from 1 to 2") {
		t.Fatal("Lucide Version value changes must be ignored")
	}
}
