//go:build !linux && !darwin

package localruntime

import "errors"

func managedPostgresIdentity() (int, int, error) {
	return 0, 0, errors.New("localruntime: managed PostgreSQL bind mounts require Linux or macOS; select databaseEnv on this platform")
}
func checkPostgresDirectoryOwner(string, int, int) error {
	return errors.New("localruntime: managed PostgreSQL bind mounts are unsupported on this platform")
}
