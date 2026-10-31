package wire

import (
	"fmt"
	"net/http"
)

type RouteLogging string

const (
	LoggingDefault RouteLogging = ""
	LoggingOff     RouteLogging = "off"
	LoggingNormal  RouteLogging = "normal"
)

func (mode RouteLogging) Resolve(method string) (RouteLogging, error) {
	switch mode {
	case LoggingDefault:
		if method == http.MethodGet || method == http.MethodHead {
			return LoggingOff, nil
		}
		return LoggingNormal, nil
	case LoggingOff, LoggingNormal:
		return mode, nil
	default:
		return "", fmt.Errorf("unknown route logging mode %q", mode)
	}
}

// ValidateRouteLogging rejects unknown policies before manifest synchronization.
func ValidateRouteLogging(manifest AgentManifest) error {
	for _, route := range manifest.Routes {
		if _, err := route.Logging.Resolve(route.Method); err != nil {
			return err
		}
	}
	return nil
}
