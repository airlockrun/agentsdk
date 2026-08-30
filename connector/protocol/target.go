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

const (
	serviceModeUser uint8 = 1 << iota
	serviceModeSystem
)

// Target defines one supported connector artifact platform and its Go build
// recipe. Connector manifests carry Target.ID values; builders own the recipe.
type Target struct {
	ID               string
	Architecture     string
	GOOS             string
	GOARCH           string
	ExecutableSuffix string

	goarm        string
	serviceModes uint8
}

var supportedTargets = [...]Target{
	{ID: PlatformLinuxAMD64, Architecture: "amd64", GOOS: "linux", GOARCH: "amd64", serviceModes: serviceModeUser | serviceModeSystem},
	{ID: PlatformLinuxARM64, Architecture: "arm64", GOOS: "linux", GOARCH: "arm64", serviceModes: serviceModeUser | serviceModeSystem},
	{ID: PlatformLinuxARMv7, Architecture: "armv7", GOOS: "linux", GOARCH: "arm", goarm: "7", serviceModes: serviceModeUser | serviceModeSystem},
	{ID: PlatformDarwinAMD64, Architecture: "amd64", GOOS: "darwin", GOARCH: "amd64", serviceModes: serviceModeUser},
	{ID: PlatformDarwinARM64, Architecture: "arm64", GOOS: "darwin", GOARCH: "arm64", serviceModes: serviceModeUser},
	{ID: PlatformWindowsAMD64, Architecture: "amd64", GOOS: "windows", GOARCH: "amd64", ExecutableSuffix: ".exe", serviceModes: serviceModeUser | serviceModeSystem},
	{ID: PlatformWindowsARM64, Architecture: "arm64", GOOS: "windows", GOARCH: "arm64", ExecutableSuffix: ".exe", serviceModes: serviceModeUser | serviceModeSystem},
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

// SupportsServiceMode reports whether target has a managed-service lifecycle
// for mode.
func (t Target) SupportsServiceMode(mode string) bool {
	switch mode {
	case "user":
		return t.serviceModes&serviceModeUser != 0
	case "system":
		return t.serviceModes&serviceModeSystem != 0
	default:
		return false
	}
}
