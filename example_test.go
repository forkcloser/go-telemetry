package telemetry_test

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/forkcloser/go-telemetry"
)

// A program's main: Watch first, then the exporter beside the handler the
// program already logs to.
func Example() { //nolint:testableexamples // Watch starts a process: a main to read, and not for go test to run
	// From wherever the program has its configuration: a flag, a file, a
	// variable of its own choosing.
	config := telemetry.Config{
		Endpoint: os.Getenv("DOOR_TELEMETRY_ENDPOINT"),
		Service:  "door",
		Version:  "1.4.0",
	}

	telemetry.Watch(config)

	exporter := telemetry.New(config)

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_ = exporter.Shutdown(ctx)
	}()

	slog.SetDefault(slog.New(slog.NewMultiHandler(
		slog.NewTextHandler(os.Stderr, nil),
		exporter.Handler(&telemetry.HandlerOptions{AddSource: true}),
	)))

	served := exporter.Counter("requests", "{request}", "Requests served.").With(slog.String("outcome", "served"))
	served.Add(1)

	slog.Info("started")
}
