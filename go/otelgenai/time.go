package otelgenai

import "time"

// now is the clock the handler uses for span timestamps when the caller
// supplies none. Tests replace now.
var now = func() time.Time { return time.Now().UTC() }
