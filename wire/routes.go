package wire

import "fmt"

// ValidateRouteTools checks correspondence against the same manifest's app
// tools. Tool names are app-local identities; access policies are independent.
func ValidateRouteTools(manifest AgentManifest) error {
	tools := make(map[string]struct{}, len(manifest.Tools))
	for _, t := range manifest.Tools {
		tools[t.Name] = struct{}{}
	}
	for _, route := range manifest.Routes {
		if route.ToolName == "" {
			continue
		}
		if _, registered := tools[route.ToolName]; !registered {
			return fmt.Errorf("route %s %s: corresponding tool %q must be registered on this app", route.Method, route.Path, route.ToolName)
		}
	}
	return nil
}
