// Package telemetry sends the logs, the metrics and the crash reports of a
// program to an OTLP/HTTP endpoint.
//
// An [Exporter] is one slog.Handler among the program's and a set of
// instruments, a [Counter], a [Gauge], a [Histogram]. It writes OTLP's
// protobuf itself and sends it through go-xhttp. It never makes the program
// wait: records go to a queue in memory, bounded, and what the endpoint
// cannot take for now is sent again later.
//
// [Watch] has the program's crashes reported by another process, since the
// one that crashes cannot report it: a panic in any goroutine, a fatal error
// of the runtime, a fault. The report is a log record like any other, with
// the runtime's text and a fingerprint that is the same for two crashes at
// the same place. [Exporter.Recovered] makes the same record of a panic the
// program recovered.
//
// Nothing is written to disk. With no endpoint in its [Config] an exporter
// does nothing and Watch starts nothing.
package telemetry
