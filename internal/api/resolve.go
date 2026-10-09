package api

// ResolveError is a model-name resolution failure the inference handler writes
// to the client as-is: status, error type, message, and Retry-After (seconds)
// when positive. It lives here rather than in internal/proxy so that a
// resolver — the alias store — can return one without importing the handler.
type ResolveError struct {
	Status     int
	Type       string
	Message    string
	RetryAfter int
}

func (e *ResolveError) Error() string { return e.Message }
