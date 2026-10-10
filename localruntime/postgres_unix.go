//go:build linux || darwin

package localruntime

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func managedPostgresIdentity() (int, int, error) {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		return 0, 0, errors.New("localruntime: managed PostgreSQL cannot run as root; run as a normal user or select databaseEnv")
	}
	return uid, gid, nil
}
func checkPostgresDirectoryOwner(path string, uid, gid int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != uid || int(stat.Gid) != gid {
		return fmt.Errorf("localruntime: PostgreSQL data directory %s must be owned by invoking UID/GID %d:%d; ownership is never changed automatically", path, uid, gid)
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("localruntime: PostgreSQL data directory must have mode 0700")
	}
	return nil
}
