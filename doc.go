// Package telemetry sends the logs and the metrics of a program to an
// OTLP/HTTP endpoint.
//
// An [Exporter] is one slog.Handler among the program's and a set of
// instruments, a [Counter], a [Gauge], a [Histogram]. It writes OTLP's
// protobuf itself and sends it through go-xhttp. It never makes the program
// wait: records go to a queue in memory, bounded, and what the endpoint
// cannot take for now is sent again later.
//
// Nothing is written to disk. With no endpoint in its [Config] an exporter
// does nothing.
package telemetry
