package wire

// UIChangeStreamPath is the same-origin, authenticated app invalidation stream.
const UIChangeStreamPath = "/__air/changes"

// UIChange contains no application data. Revision is an opaque equality token,
// not a counter or an exactly-once delivery guarantee.
type UIChange struct {
	Topic    string `json:"topic"`
	Revision string `json:"revision"`
}
