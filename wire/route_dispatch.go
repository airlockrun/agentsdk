package wire

import "time"

const RouteDispatchHeader = "X-Airlock-Route-Dispatch"
const RouteDeadlineHeader = "X-Airlock-Route-Deadline"

// MaterializeRouteRequest carries only the opaque host-signed dispatch proof.
// Caller snapshots and request payloads are never accepted as authority.
type MaterializeRouteRequest struct {
	Proof string `json:"proof"`
}
type MaterializeRouteResponse struct {
	Runtime  RuntimeContext `json:"runtime"`
	Deadline time.Time      `json:"deadline"`
	Logging  RouteLogging   `json:"logging"`
}
