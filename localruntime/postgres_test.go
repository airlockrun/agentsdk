package localruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
)

func TestManagedPostgresBindMountPersistenceAndIsolation(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	if _, _, err := managedPostgresIdentity(); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	configs := []PostgresConfig{{AppID: uuid.NewString(), Directory: filepath.Join(t.TempDir(), "PG data one")}, {AppID: uuid.NewString(), Directory: filepath.Join(t.TempDir(), "PG data two")}}
	handles := make([]*ManagedPostgres, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range configs {
		wg.Add(1)
		go func() { defer wg.Done(); handles[i], errs[i] = StartManagedPostgres(ctx, configs[i]) }()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
		h := handles[i]
		t.Cleanup(func() {
			if err := h.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	first, second := handles[0], handles[1]
	u1, _ := url.Parse(first.DSN)
	u2, _ := url.Parse(second.DSN)
	if u1.Host == u2.Host {
		t.Fatal("per-app databases share a port")
	}
	meta, err := inspectPostgres(ctx, first.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, _ := managedPostgresIdentity()
	if err := verifyPostgresContainer(meta, first.config, uid, gid); err != nil {
		t.Fatal(err)
	}
	if _, err := StartManagedPostgres(ctx, configs[0]); err == nil {
		t.Fatal("two live containers accepted one data directory")
	}
	db, err := sql.Open("postgres", first.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE retained (value text NOT NULL); INSERT INTO retained VALUES ('durable fixture')`); err != nil {
		t.Fatal(err)
	}
	var super, createdb, createrole, replication bool
	if err := db.QueryRowContext(ctx, `SELECT rolsuper,rolcreatedb,rolcreaterole,rolreplication FROM pg_roles WHERE rolname=current_user`).Scan(&super, &createdb, &createrole, &replication); err != nil {
		t.Fatal(err)
	}
	if super || createdb || createrole || replication {
		t.Fatal("app role has global privileges")
	}
	db.Close()
	oldID := first.ContainerID
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.CommandContext(ctx, "docker", "container", "inspect", oldID).Output(); err == nil {
		t.Fatal("ephemeral PostgreSQL container remains after close")
	}
	if err := checkPostgresDirectoryOwner(filepath.Join(first.Directory, "pgdata"), uid, gid); err != nil {
		t.Fatal(err)
	}
	version, err := os.ReadFile(filepath.Join(first.Directory, "pgdata", "PG_VERSION"))
	if err != nil || strings.TrimSpace(string(version)) != "17" {
		t.Fatalf("retained PG version %q %v", version, err)
	}
	info, err := os.Stat(filepath.Join(first.Directory, "credentials.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential record is not private")
	}
	restarted, err := StartManagedPostgres(ctx, configs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.ContainerID == oldID {
		t.Fatal("container was reused instead of recreated")
	}
	db, err = sql.Open("postgres", restarted.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRowContext(ctx, `SELECT value FROM retained`).Scan(&value); err != nil || value != "durable fixture" {
		t.Fatalf("restart value=%q %v", value, err)
	}
	changed := configs[0]
	changed.AppID = uuid.NewString()
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := StartManagedPostgres(ctx, changed); err == nil {
		t.Fatal("another app borrowed the data directory")
	}
}

func TestManagedPostgresRefusesForeignContainer(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	uid, _, err := managedPostgresIdentity()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	name := postgresContainerName(directory, uid)
	if _, err := dockerOutput(ctx, "create", "--name", name, ManagedPostgresImage); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = dockerOutput(context.Background(), "rm", "--force", name) })
	if _, err := StartManagedPostgres(ctx, PostgresConfig{AppID: uuid.NewString(), Directory: directory}); err == nil {
		t.Fatal("foreign named container accepted")
	}
	meta, err := inspectPostgres(ctx, name)
	if err != nil || meta.ID == "" {
		t.Fatal("foreign container was removed")
	}
}

func TestManagedPostgresRecoversExactStaleContainer(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	if _, _, err := managedPostgresIdentity(); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	config := PostgresConfig{AppID: uuid.NewString(), Directory: directory}
	first, err := StartManagedPostgres(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	db, err := sql.Open("postgres", first.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE crash_retained (value text NOT NULL); INSERT INTO crash_retained VALUES ('known state')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Process death releases its file lock but leaves Docker work running. This
	// fixture releases that same ownership lease without stopping the container.
	if err := first.lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	recovered, err := StartManagedPostgres(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if recovered.ContainerID == first.ContainerID {
		t.Fatal("stale container was not replaced")
	}
	if _, err := exec.CommandContext(ctx, "docker", "container", "inspect", first.ContainerID).Output(); err == nil {
		t.Fatal("exact stale container was not removed")
	}
	db, err = sql.Open("postgres", recovered.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRowContext(ctx, `SELECT value FROM crash_retained`).Scan(&value); err != nil || value != "known state" {
		t.Fatalf("recovered data=%q %v", value, err)
	}
}

func TestManagedPostgresCredentialValidation(t *testing.T) {
	dir := t.TempDir()
	config := PostgresConfig{AppID: uuid.NewString(), Directory: dir, Image: ManagedPostgresImage}
	secrets := postgresSecrets{AppID: config.AppID, Image: config.Image, UID: 123, GID: 456, AdminPassword: strings.Repeat("a", 64), AppPassword: strings.Repeat("b", 64)}
	raw, _ := json.Marshal(secrets)
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPostgresSecrets(config, 123, 456); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPostgresSecrets(config, 124, 456); err == nil {
		t.Fatal("mismatched UID accepted")
	}
	secrets.AppPassword = strings.Repeat("'", 64)
	raw, _ = json.Marshal(secrets)
	os.WriteFile(filepath.Join(dir, "credentials.json"), raw, 0600)
	if _, err := loadPostgresSecrets(config, 123, 456); err == nil {
		t.Fatal("invalid password bytes accepted")
	}
}
