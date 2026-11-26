# go-telemetry

The logs and the metrics of a Go program, sent to an OTLP/HTTP endpoint. The program keeps logging with `log/slog`; nothing is
written to disk, and nothing makes the program wait.

```go
import "github.com/forkcloser/go-telemetry"
```

## Logs and metrics

```go
exporter := telemetry.New(telemetry.Config{
	Endpoint: endpoint, // from wherever the program has its configuration
	Service:  "door",
	Version:  version,
})
defer exporter.Shutdown(ctx)

slog.SetDefault(slog.New(slog.NewMultiHandler(
	terminal,                                      // what the program logs to today
	exporter.Handler(&telemetry.HandlerOptions{}), // the same records, as OTLP
)))

requests := exporter.Counter("http.server.requests", "{request}", "Requests served.")
served := requests.With(slog.String("outcome", "served")) // a series, as a logger's With is a logger
served.Add(1)

exporter.Gauge("queue.length", "{item}", "Items waiting.").Set(12)
exporter.Histogram("http.server.request.duration", "s", "How long.", 0.01, 0.1, 1).Record(0.042)
```

With no endpoint nothing is exported and nothing runs: the handler is never
enabled and the instruments count nothing.

The library reads no configuration of its own, and none from the
environment: a `Config` is the program's to fill, from a flag, a file or a
variable of its choosing.

What an exporter promises:

- **Logging never waits on the endpoint.** A record goes into a queue in
  memory and the call returns.
- **What the endpoint cannot take for now is kept.** When the endpoint is out
  of reach, or answers 429, 502, 503 or 504, the batch goes back to the queue
  and is sent again at the next interval, in order.
- **The queue has a cap:** 2048 records by default (`Config.Queue`) and
  16 MiB. Past it the oldest records are dropped, so what the endpoint finds
  when it is back is the latest.
- **A batch refused outright is dropped.** Any other status than those four
  would be the same the next time.
- **What is dropped is seen.** `telemetry.log.dropped` and
  `telemetry.export.failed` are sent as metrics with the rest, and `Shutdown`
  returns how many records were still waiting when the program stopped.
- **A request is small:** 512 records at most, and 4 MiB of them.
- **Metrics lose nothing to a failed request:** the sums are cumulative.

The requests go through [go-xhttp](https://github.com/forkcloser/go-xhttp):
https with a post-quantum hybrid key exchange unless the program hands
`Config.Client` another client.

## The files

| File | Holds |
|---|---|
| `wire.go` | A protobuf writer for OTLP's few messages: no generated code, no reflection |
| `value.go` | slog values and attributes as OTLP's `AnyValue` and `KeyValue` |
| `handler.go` | The `slog.Handler`: message as body, level as severity, a group as a map |
| `metrics.go` | Counter, gauge, histogram; cumulative; 2000 series an instrument at most |
| `exporter.go` | Configuration, the bounded queue, batches, the request |
| `const.go`, `errors.go` | The exported constants, and the one sentinel |

The library imports the standard library and go-xhttp.
The tests alone import OpenTelemetry's generated types, to decode what the
library sent.

## What is not built

- gzip on the requests.
- Runtime metrics (goroutines, memory, garbage collection).
- OTLP's partial-success answers: a 2xx is taken as "all of it".
- Traces.
