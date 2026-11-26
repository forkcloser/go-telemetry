package telemetry

import "errors"

// ErrExport is the error of a request the endpoint did not take, as OnError
// is told it, and of a Shutdown that left records unsent.
var ErrExport = errors.New("the telemetry endpoint did not take the batch")
