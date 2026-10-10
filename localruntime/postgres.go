package localruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// ManagedPostgresImage pins the PostgreSQL 17 pgvector amd64/arm64 manifest.
const ManagedPostgresImage = "pgvector/pgvector@sha256:ac08538c6f8b9904c33c8224c5e5706dbe760aca29db1d096972b4052c22a75d"
const localAppLabel = "run.airlock.local.app"
const localPurposeLabel = "run.airlock.local.purpose"
const localDataLabel = "run.airlock.local.data"

// Directory contains credentials.json and pgdata/. The container is ephemeral;
// neither database bytes nor credentials are kept in a Docker named volume.
type PostgresConfig struct{ AppID, Directory, Image string }
type postgresSecrets struct {
	AppID, Image, AdminPassword, AppPassword string
	UID, GID                                 int
}
type postgresContainer struct {
	ID     string
	Config struct {
		Image, User string
		Labels      map[string]string
	}
	HostConfig      struct{ AutoRemove bool }
	Mounts          []struct{ Type, Source, Destination string }
	NetworkSettings struct {
		Ports map[string][]struct{ HostIP, HostPort string }
	}
}

type ManagedPostgres struct {
	DSN, ContainerID, Directory string
	config                      PostgresConfig
	lock                        *flock.Flock
	uid, gid                    int
	once                        sync.Once
	closeErr                    error
}

func dockerOutput(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("localruntime: Docker %s failed: %w: %s", args[0], err, out)
	}
	return out, nil
}
func inspectPostgres(ctx context.Context, id string) (postgresContainer, error) {
	raw, err := dockerOutput(ctx, "container", "inspect", id)
	if err != nil {
		return postgresContainer{}, err
	}
	var values []postgresContainer
	if json.Unmarshal(raw, &values) != nil || len(values) != 1 {
		return postgresContainer{}, errors.New("localruntime: invalid PostgreSQL container metadata")
	}
	return values[0], nil
}
func postgresContainerName(directory string, uid int) string {
	hash := sha256.Sum256([]byte(directory + "\x00" + strconv.Itoa(uid)))
	return "airlock-local-pg-" + hex.EncodeToString(hash[:12])
}

func StartManagedPostgres(ctx context.Context, config PostgresConfig) (result *ManagedPostgres, err error) {
	id, parseErr := uuid.Parse(config.AppID)
	if parseErr != nil || id == uuid.Nil || id.String() != config.AppID || config.Directory == "" {
		return nil, errors.New("localruntime: managed PostgreSQL requires a canonical app UUID and dedicated host data directory")
	}
	uid, gid, err := managedPostgresIdentity()
	if err != nil {
		return nil, err
	}
	if config.Image == "" {
		config.Image = ManagedPostgresImage
	}
	if !strings.Contains(config.Image, "@sha256:") {
		return nil, errors.New("localruntime: PostgreSQL image must be digest-pinned")
	}
	absolute, err := filepath.Abs(config.Directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	config.Directory, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	if err := checkPostgresDirectoryOwner(config.Directory, uid, gid); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(config.Directory, "runtime.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !locked {
		return nil, errors.New("localruntime: this PostgreSQL data directory is already in use")
	}
	h := &ManagedPostgres{Directory: config.Directory, config: config, lock: lock, uid: uid, gid: gid}
	defer func() {
		if err != nil {
			err = errors.Join(err, h.Close())
		}
	}()
	if _, err := dockerOutput(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return nil, err
	}
	if _, err := exec.CommandContext(ctx, "docker", "image", "inspect", config.Image).Output(); err != nil {
		if _, err := dockerOutput(ctx, "pull", config.Image); err != nil {
			return nil, err
		}
	}
	name := postgresContainerName(config.Directory, uid)
	// Holding the lifetime file lock proves that any exact owned container left
	// at this name is stale. Unknown containers are never stopped or removed.
	raw, err := dockerOutput(ctx, "ps", "--all", "--filter", "name=^/"+name+"$", "--format", "{{json .ID}}")
	if err != nil {
		return nil, err
	}
	staleID := strings.Trim(strings.TrimSpace(string(raw)), "\"")
	if staleID != "" {
		meta, err := inspectPostgres(ctx, staleID)
		if err != nil {
			return nil, err
		}
		if err := verifyPostgresContainer(meta, config, uid, gid); err != nil {
			return nil, err
		}
		if _, err := os.Stat(filepath.Join(config.Directory, "credentials.json")); err != nil {
			return nil, errors.New("localruntime: stale PostgreSQL container has no persisted credential record")
		}
		if _, err := dockerOutput(ctx, "rm", "--force", meta.ID); err != nil {
			return nil, err
		}
	}
	secrets, err := loadPostgresSecrets(config, uid, gid)
	if err != nil {
		return nil, err
	}
	pgdata := filepath.Join(config.Directory, "pgdata")
	if err := os.MkdirAll(pgdata, 0700); err != nil {
		return nil, err
	}
	if err := checkPostgresDirectoryOwner(pgdata, uid, gid); err != nil {
		return nil, err
	}
	if version, err := os.ReadFile(filepath.Join(pgdata, "PG_VERSION")); err == nil && strings.TrimSpace(string(version)) != "17" {
		return nil, errors.New("localruntime: managed PostgreSQL data is not version 17; migrate it explicitly")
	}
	env, err := os.CreateTemp(config.Directory, ".postgres-env-")
	if err != nil {
		return nil, err
	}
	envPath := env.Name()
	defer os.Remove(envPath)
	_, err = env.WriteString("POSTGRES_USER=postgres\nPOSTGRES_DB=postgres\nPOSTGRES_PASSWORD=" + secrets.AdminPassword + "\n")
	err = errors.Join(err, env.Chmod(0600), env.Close())
	if err != nil {
		return nil, err
	}
	if strings.Contains(pgdata, ",") {
		return nil, errors.New("localruntime: PostgreSQL bind-mount directory must not contain a comma")
	}
	socketMount := fmt.Sprintf("/var/run/postgresql:rw,nosuid,noexec,size=4m,uid=%d,gid=%d,mode=0700", uid, gid)
	out, err := dockerOutput(ctx, "create", "--pull", "never", "--rm", "--name", name, "--user", strconv.Itoa(uid)+":"+strconv.Itoa(gid), "--tmpfs", socketMount, "--label", localAppLabel+"="+config.AppID, "--label", localPurposeLabel+"=postgres", "--label", localDataLabel+"="+config.Directory, "--env-file", envPath, "--publish", "127.0.0.1::5432", "--mount", "type=bind,src="+pgdata+",dst=/var/lib/postgresql/data", config.Image)
	if err != nil {
		return nil, err
	}
	h.ContainerID = strings.TrimSpace(string(out))
	if _, err := dockerOutput(ctx, "start", h.ContainerID); err != nil {
		return nil, err
	}
	meta, err := inspectPostgres(ctx, h.ContainerID)
	if err != nil {
		return nil, err
	}
	if err := verifyPostgresContainer(meta, config, uid, gid); err != nil {
		return nil, err
	}
	bindings := meta.NetworkSettings.Ports["5432/tcp"]
	if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" {
		return nil, errors.New("localruntime: PostgreSQL must bind one Docker-assigned loopback port")
	}
	endpoint := "127.0.0.1:" + bindings[0].HostPort
	admin, err := sql.Open("postgres", postgresDSN(endpoint, "postgres", "postgres", secrets.AdminPassword))
	if err != nil {
		return nil, err
	}
	defer admin.Close()
	ready, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		if admin.PingContext(ready) == nil {
			break
		}
		select {
		case <-ready.Done():
			return nil, errors.New("localruntime: PostgreSQL did not become ready; check bind-mount ownership and container initialization")
		case <-time.After(100 * time.Millisecond):
		}
	}
	if err := bootstrapPostgres(ready, admin, secrets.AppPassword); err != nil {
		return nil, err
	}
	appAdmin, err := sql.Open("postgres", postgresDSN(endpoint, "postgres", "airlock_app", secrets.AdminPassword))
	if err != nil {
		return nil, err
	}
	defer appAdmin.Close()
	if _, err := appAdmin.ExecContext(ready, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return nil, err
	}
	h.DSN = postgresDSN(endpoint, "airlock_app", "airlock_app", secrets.AppPassword)
	app, err := sql.Open("postgres", h.DSN)
	if err != nil {
		return nil, err
	}
	defer app.Close()
	if app.PingContext(ready) != nil {
		return nil, errors.New("localruntime: persisted app PostgreSQL credential failed authentication")
	}
	return h, nil
}

func loadPostgresSecrets(config PostgresConfig, uid, gid int) (postgresSecrets, error) {
	name := filepath.Join(config.Directory, "credentials.json")
	raw, err := os.ReadFile(name)
	var secrets postgresSecrets
	if err == nil {
		if json.Unmarshal(raw, &secrets) != nil || secrets.AppID != config.AppID || secrets.Image != config.Image || secrets.UID != uid || secrets.GID != gid {
			return secrets, errors.New("localruntime: PostgreSQL data directory app, image or UID/GID differs from its persisted owner")
		}
		for _, password := range []string{secrets.AdminPassword, secrets.AppPassword} {
			decoded, err := hex.DecodeString(password)
			if err != nil || len(decoded) != 32 {
				return secrets, errors.New("localruntime: invalid persisted PostgreSQL credential")
			}
		}
		info, err := os.Stat(name)
		if err != nil {
			return secrets, err
		}
		if info.Mode().Perm() != 0600 {
			return secrets, errors.New("localruntime: PostgreSQL credentials.json must have mode 0600")
		}
		return secrets, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return secrets, err
	}
	entries, err := os.ReadDir(config.Directory)
	if err != nil {
		return secrets, err
	}
	for _, entry := range entries {
		if entry.Name() != "runtime.lock" {
			return secrets, errors.New("localruntime: new PostgreSQL data directory must be empty except for its runtime lock")
		}
	}
	secrets = postgresSecrets{AppID: config.AppID, Image: config.Image, AdminPassword: randomPostgresPassword(), AppPassword: randomPostgresPassword(), UID: uid, GID: gid}
	raw, err = json.Marshal(secrets)
	if err != nil {
		return secrets, err
	}
	return secrets, atomicCacheFile(config.Directory, "credentials.json", raw, 0600)
}
func randomPostgresPassword() string {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}
func postgresDSN(endpoint, user, database, password string) string {
	return (&url.URL{Scheme: "postgres", Host: endpoint, User: url.UserPassword(user, password), Path: "/" + database, RawQuery: "sslmode=disable"}).String()
}
func verifyPostgresContainer(meta postgresContainer, config PostgresConfig, uid, gid int) error {
	if meta.Config.Image != config.Image || meta.Config.User != strconv.Itoa(uid)+":"+strconv.Itoa(gid) || !meta.HostConfig.AutoRemove || meta.Config.Labels[localAppLabel] != config.AppID || meta.Config.Labels[localPurposeLabel] != "postgres" || meta.Config.Labels[localDataLabel] != config.Directory {
		return errors.New("localruntime: PostgreSQL container ownership, image, UID/GID or auto-removal mismatch")
	}
	for _, mount := range meta.Mounts {
		if mount.Type == "bind" && mount.Source == filepath.Join(config.Directory, "pgdata") && mount.Destination == "/var/lib/postgresql/data" {
			return nil
		}
	}
	return errors.New("localruntime: PostgreSQL container does not use its owned host bind mount")
}
func bootstrapPostgres(ctx context.Context, admin *sql.DB, password string) error {
	var exists bool
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_roles WHERE rolname='airlock_app')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, `CREATE ROLE airlock_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '`+password+`'`); err != nil {
			return err
		}
	}
	var super, createdb, createrole, replication bool
	if err := admin.QueryRowContext(ctx, `SELECT rolsuper,rolcreatedb,rolcreaterole,rolreplication FROM pg_roles WHERE rolname='airlock_app'`).Scan(&super, &createdb, &createrole, &replication); err != nil {
		return err
	}
	if super || createdb || createrole || replication {
		return errors.New("localruntime: managed app database role has unexpected global privileges")
	}
	if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT FROM pg_database WHERE datname='airlock_app')`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := admin.ExecContext(ctx, `CREATE DATABASE airlock_app OWNER airlock_app`); err != nil {
			return err
		}
	}
	var owner string
	if err := admin.QueryRowContext(ctx, `SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='airlock_app'`).Scan(&owner); err != nil {
		return err
	}
	if owner != "airlock_app" {
		return errors.New("localruntime: managed app database owner mismatch")
	}
	return nil
}

// Close drains only this verified container, confirms auto-removal, then releases
// the lifetime lock. Host data and credentials are retained for the next run.
func (p *ManagedPostgres) Close() error {
	p.once.Do(func() {
		defer func() { p.closeErr = errors.Join(p.closeErr, p.lock.Unlock()) }()
		if p.ContainerID == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		raw, err := dockerOutput(ctx, "ps", "--all", "--filter", "id="+p.ContainerID, "--format", "{{.ID}}")
		if err != nil {
			p.closeErr = err
			return
		}
		if strings.TrimSpace(string(raw)) == "" {
			return
		}
		meta, err := inspectPostgres(ctx, p.ContainerID)
		if err != nil {
			p.closeErr = err
			return
		}
		if err := verifyPostgresContainer(meta, p.config, p.uid, p.gid); err != nil {
			p.closeErr = err
			return
		}
		if _, err := dockerOutput(ctx, "stop", "--time", "10", p.ContainerID); err != nil {
			p.closeErr = err
			return
		}
		for {
			out, err := dockerOutput(ctx, "ps", "--all", "--filter", "id="+p.ContainerID, "--format", "{{.ID}}")
			if err != nil {
				p.closeErr = err
				return
			}
			if strings.TrimSpace(string(out)) == "" {
				return
			}
			select {
			case <-ctx.Done():
				p.closeErr = errors.New("localruntime: PostgreSQL container auto-removal did not complete")
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	})
	return p.closeErr
}
