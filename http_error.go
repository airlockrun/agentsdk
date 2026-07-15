package agentsdk

import "fmt"

// HTTPError carries a safe response and an internal cause from a route handler.
// The SDK writes Message to the caller and records/logs Cause.
type HTTPError struct {
	Status  int
	Message string
	Cause   error
}

// NewHTTPError constructs an HTTPError for a 4xx or 5xx response.
func NewHTTPError(status int, message string, cause error) *HTTPError {
	if status < 400 || status > 599 {
		panic(fmt.Sprintf("agentsdk: NewHTTPError status %d is not 4xx or 5xx", status))
	}
	if message == "" {
		panic("agentsdk: NewHTTPError message is required")
	}
	return &HTTPError{Status: status, Message: message, Cause: cause}
}

func (e *HTTPError) Error() string {
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return e.Message
}

func (e *HTTPError) Unwrap() error { return e.Cause }
