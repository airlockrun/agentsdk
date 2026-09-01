package protocol

const (
	PlatformLinuxAMD64   = "linux-amd64"
	PlatformLinuxARM64   = "linux-arm64"
	PlatformLinuxARMv7   = "linux-armv7"
	PlatformDarwinAMD64  = "darwin-amd64"
	PlatformDarwinARM64  = "darwin-arm64"
	PlatformWindowsAMD64 = "windows-amd64"
	PlatformWindowsARM64 = "windows-arm64"
)

// Target defines one supported connector artifact platform and its Go build
// recipe. Connector manifests carry Target.ID values; builders own the recipe.
type Target struct {
	ID               string
	Architecture     string
	GOOS             string
	GOARCH           string
	ExecutableSuffix string

	goarm string
}

var supportedTargets = [...]Target{
	{ID: PlatformLinuxAMD64, Architecture: "amd64", GOOS: "linux", GOARCH: "amd64"},
	{ID: PlatformLinuxARM64, Architecture: "arm64", GOOS: "linux", GOARCH: "arm64"},
	{ID: PlatformLinuxARMv7, Architecture: "armv7", GOOS: "linux", GOARCH: "arm", goarm: "7"},
	{ID: PlatformDarwinAMD64, Architecture: "amd64", GOOS: "darwin", GOARCH: "amd64"},
	{ID: PlatformDarwinARM64, Architecture: "arm64", GOOS: "darwin", GOARCH: "arm64"},
	{ID: PlatformWindowsAMD64, Architecture: "amd64", GOOS: "windows", GOARCH: "amd64", ExecutableSuffix: ".exe"},
	{ID: PlatformWindowsARM64, Architecture: "arm64", GOOS: "windows", GOARCH: "arm64", ExecutableSuffix: ".exe"},
}

// LookupTarget returns the registered build target for id.
func LookupTarget(id string) (Target, bool) {
	for _, target := range supportedTargets {
		if target.ID == id {
			return target, true
		}
	}
	return Target{}, false
}

// SupportedTargets returns every registered target in stable order.
func SupportedTargets() []Target {
	return append([]Target(nil), supportedTargets[:]...)
}

// GoEnv returns a fresh Go environment fragment for cross-compiling the target.
func (t Target) GoEnv() []string {
	environment := []string{"GOOS=" + t.GOOS, "GOARCH=" + t.GOARCH}
	if t.goarm != "" {
		environment = append(environment, "GOARM="+t.goarm)
	}
	return environment
}
