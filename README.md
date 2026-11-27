# go-telemetry

The logs, the metrics and the crash reports of a Go program, sent to an
OTLP/HTTP endpoint. The program keeps logging with `log/slog`; nothing is
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

Every request goes through [go-xhttp](https://github.com/forkcloser/go-xhttp),
and through nothing else: the exporter makes a client of its own under
`Config.Transport`, which is go-xhttp's configuration. Its zero value is
https with a post-quantum hybrid key exchange only, and an endpoint that needs
less is one the program says so of:

```go
telemetry.Config{
	Endpoint:  "http://collector.internal:4318",
	Service:   "door",
	Transport: xhttp.Config{Cleartext: true},
}
```

A program hands the exporter no client of its own and shares no connection
with it. An endpoint its `Transport` sends nothing to, an `http` one without
`Cleartext`, is a mistake `New` and `Watch` panic on at the start, like an
endpoint that is no URL: it would fail the same way at every request.

## Crash reports

A program calls `Watch` first in `main`:

```go
func main() {
	config := telemetry.Config{Endpoint: endpoint, Service: "door", Version: version}
	telemetry.Watch(config)
	// … the program
}
```

A crash is reported by another process, the **watcher**, since the process
that crashes cannot report it. `Watch` starts the same binary a second time,
and which of the two processes is the watcher depends on where the program
runs:

| Where | The watcher | The program |
|---|---|---|
| Linux, macOS, Windows, started by anything | the second process, set apart | the process that was started |
| Linux, as the first process of a container | the first process | the second process |

**Beside the program.** The program stays the process that was started: its
process number, its signals, its terminal, its exit status and whatever
started it are untouched. The watcher is in a session of its own, holds none
of the program's files but its standard error, and ends when the program
does. It keeps that standard error open until the report of a crash is
delivered or given up: after a crash, whatever reads the program's errors to
their end, a shell's `$(…)`, a CI step, waits that long.

**As the first process of a container.** When the first process of a
container ends the kernel kills every other, a watcher among them, so there
the first process is the watcher and the program its child. The watcher does
what the first process of a container is there to do: it sends the program
every signal it is sent, gives it the terminal when there is one, waits for
the processes left to it, and exits as the program did, with its exit code or
with 128 and the number of the signal that killed it. It keeps none of the
files the container was started with, nor the program's standard input and
output: a socket the program listened on is free when the program ends.

What is reported:

| How the program ended | Beside the program | As the first process |
|---|---|---|
| a panic, in any goroutine | yes | yes |
| a fatal error of the runtime (a map written twice at once, a deadlock) | yes | yes |
| a panic the program recovered and reported itself (`Exporter.Recovered`) | yes | yes |
| killed by a signal nobody sent on purpose, the kernel's kill for memory among them | no | yes |
| an exit with the status of a fault (Windows) | yes | |
| an exit, whatever its code; an end by SIGINT, SIGTERM, SIGHUP or SIGPIPE | no | no |

The record is a log record, FATAL, its body the type and the message. It
carries:

- `exception.type`, `exception.message` (one line, 1 KiB at most),
  `exception.stacktrace` (what the runtime printed, its first 64 KiB);
- `crash.fingerprint`, 32 hexadecimal digits that are the same for two
  crashes at the same place: made of the kind and the function names of the
  goroutine that crashed, and of nothing that varies between runs. The same
  fault has the same one whether it ended the program or was recovered. A
  record that has this attribute is a crash;
- `process.pid`, and `crash.signal` or `process.exit.code` where the watcher
  is told how the program ended;
- `crash.stacktrace.cut`, the bytes left out of a text longer than 64 KiB.

The watcher takes 30 seconds at most to deliver (`Config.CrashDelivery`),
sending again while the endpoint cannot take the report for now. Past that
the report is lost, and standard error says so. When the endpoint is away
that is how long the watcher goes on after a crash: as the first process of
a container it holds up the exit, and so the restart, that long; beside the
program it holds the program's standard error open that long.

With no endpoint in its `Config`, `Watch` starts nothing and there is one
process: a program that is not to be watched, under a debugger say, gives it
none, or does not call it.

`Watch` tells the process it starts what that process is through one
variable of its own, `_TELEMETRY_INTERNAL_ROLE`, which that process takes out
of its environment as it reads it. It is internal: no configuration, no part
of what the library offers, and its name and content may change. It is named
here because it is the one thing the library reads of the environment. Set
alone it changes nothing: a process takes a role only when it was also
started with what `Watch` starts it with, a pipe under the number the role
names.

### Limits

- **The watcher must outlive the program.** Where something ends every
  process with the program, a container whose first process is another
  (`docker run --init`, a shell that starts the program), a Windows job
  object that is closed, the report is sent only if it is sent by then.
- **The reason of a runtime fatal error is not in the report.** Go prints
  "fatal error: …" before it starts copying what it prints to the crash
  output, so the report has the stack and where it happened, and the reason
  is on standard error alone.
- **The last logs before a crash are lost:** what the program's exporter had
  queued dies with it, up to one interval of records.
- **`Watch` runs `main` twice up to its call.** What the program does before
  it, it does in both processes: call it first.
- **Windows containers** are not handled as Linux ones are.

## The files

| File | Holds |
|---|---|
| `wire.go` | A protobuf writer for OTLP's few messages: no generated code, no reflection |
| `value.go` | slog values and attributes as OTLP's `AnyValue` and `KeyValue` |
| `handler.go` | The `slog.Handler`: message as body, level as severity, a group as a map |
| `metrics.go` | Counter, gauge, histogram; cumulative; 2000 series an instrument at most |
| `exporter.go` | Configuration, the bounded queue, batches, the request |
| `crash.go` | The crash record, the fingerprint, `Recovered` |
| `watch.go` | `Watch`, the watcher beside a program, the delivery |
| `watch_unix.go`, `watch_windows.go` | Starting the watcher beside a program, on each platform |
| `watch_linux.go` | The watcher as the first process of a container |
| `const.go`, `errors.go` | The exported constants, and the one sentinel |

The library imports the standard library, go-xhttp and `golang.org/x/sys`.
The tests alone import OpenTelemetry's generated types, to decode what the
library sent.

## What is not built

- gzip on the requests.
- Runtime metrics (goroutines, memory, garbage collection).
- OTLP's partial-success answers: a 2xx is taken as "all of it".
- Traces.
