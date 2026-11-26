package telemetry_test

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/forkcloser/go-xhttp"

	"github.com/forkcloser/go-telemetry"
)

// TestARequestIsBoundedInBytes: 4 MiB of records at most, whatever their
// count.
func TestARequestIsBoundedInBytes(t *testing.T) {
	t.Parallel()

	got := receive(t)

	config := got.config()
	config.Interval = time.Hour

	exporter := telemetry.New(config)
	logger := slog.New(exporter.Handler(nil))

	const logged, each = 40, 200 << 10

	for range logged {
		logger.Info("a large record", "weight", strings.Repeat("x", each))
	}

	shutdown(t, exporter)

	if received := len(got.records()); received != logged {
		t.Fatalf("%d records received, want %d", received, logged)
	}

	got.mu.Lock()
	defer got.mu.Unlock()

	for _, request := range got.logs {
		size := 0

		for _, record := range request.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords() {
			size += proto.Size(record)
		}

		if size > 4<<20 {
			t.Errorf("a request of %d bytes of records, want 4 MiB at most", size)
		}
	}
}

// TestWhatIsSentAgainKeepsItsPlace: a batch the endpoint could not take goes
// back before what was logged while it was out.
func TestWhatIsSentAgainKeepsItsPlace(t *testing.T) {
	t.Parallel()

	got := receive(t)
	got.setAnswer(http.StatusServiceUnavailable)

	release := got.holding()

	defer release()

	config := got.config()
	config.Interval = time.Hour

	exporter := telemetry.New(config)
	logger := slog.New(exporter.Handler(nil))

	// A whole batch, which is sent without waiting for the interval.
	const batch = 512

	for index := range batch {
		logger.Info("of the batch", "index", index)
	}

	// The batch is out, and refused once the stand-in answers.
	got.waitAsked(t)
	logger.Info("logged meanwhile", "index", batch)

	got.setAnswer(0)
	release()

	shutdown(t, exporter)

	reported := got.records()
	if len(reported) != batch+1 {
		t.Fatalf("%d records received, want %d", len(reported), batch+1)
	}

	for place, record := range reported {
		if index := whole(mapped(record.GetAttributes())["index"]); index != int64(place) {
			t.Fatalf("the record logged as number %d arrived as number %d", index, place)
		}
	}
}

// TestAFullBatchDoesNotWaitForTheInterval: 512 records waiting are sent at
// once.
func TestAFullBatchDoesNotWaitForTheInterval(t *testing.T) {
	t.Parallel()

	got := receive(t)

	config := got.config()
	config.Interval = time.Hour

	exporter := telemetry.New(config)
	defer shutdown(t, exporter)

	logger := slog.New(exporter.Handler(nil))

	for range 512 {
		logger.Info("one of a batch")
	}

	got.waitAsked(t)
}

// TestARecordTooLargeIsDroppedAndCounted: one of more than 256 KiB.
func TestARecordTooLargeIsDroppedAndCounted(t *testing.T) {
	t.Parallel()

	got := receive(t)
	exporter := telemetry.New(got.config())
	logger := slog.New(exporter.Handler(nil))

	logger.Info("too large", "weight", strings.Repeat("x", 300<<10))
	logger.Info("small")

	shutdown(t, exporter)

	reported := got.records()
	if len(reported) != 1 || reported[0].GetBody().GetStringValue() != "small" || got.dropped() != 1 {
		t.Errorf(
			"%d records received and %d counted as dropped, want the small one and 1",
			len(reported),
			got.dropped(),
		)
	}
}

// TestWhatIsLoggedAfterShutdownGoesNowhere: it is not kept either, and a
// second Shutdown has nothing to say of it.
func TestWhatIsLoggedAfterShutdownGoesNowhere(t *testing.T) {
	t.Parallel()

	got := receive(t)
	exporter := telemetry.New(got.config())
	logger := slog.New(exporter.Handler(nil))

	shutdown(t, exporter)

	logger.Info("after the end")

	if err := exporter.Shutdown(t.Context()); err != nil || len(got.records()) != 0 {
		t.Errorf("a second Shutdown: %v, with %d records received; want nil and none", err, len(got.records()))
	}
}

// TestTheHeadersOfAConfigAreOnEveryRequest: logs and metrics alike.
func TestTheHeadersOfAConfigAreOnEveryRequest(t *testing.T) {
	t.Parallel()

	got := receive(t)

	config := got.config()
	config.Headers = http.Header{"Authorization": {"Bearer token"}, "X-Scope": {"house"}}

	exporter := telemetry.New(config)

	slog.New(exporter.Handler(nil)).Info("a record")
	exporter.Counter("requests", "", "").Add(1)

	shutdown(t, exporter)

	got.mu.Lock()
	defer got.mu.Unlock()

	if len(got.headers) < 2 {
		t.Fatalf("%d requests received, want one of logs and one of metrics", len(got.headers))
	}

	for _, headers := range got.headers {
		if headers.Get("Authorization") != "Bearer token" || headers.Get("X-Scope") != "house" {
			t.Errorf("a request with the headers %v", headers)
		}
	}
}

// TestTheResourceNamesTheInstanceOrTheHost: a Config with an Instance is
// known by it, and one with none by the host's name.
func TestTheResourceNamesTheInstanceOrTheHost(t *testing.T) {
	t.Parallel()

	resource := func(instance string) map[string]any {
		got := receive(t)

		config := got.config()
		config.Instance = instance

		exporter := telemetry.New(config)

		slog.New(exporter.Handler(nil)).Info("a record")
		shutdown(t, exporter)

		got.mu.Lock()
		defer got.mu.Unlock()

		if len(got.logs) != 1 {
			t.Fatalf("%d requests of logs, want 1", len(got.logs))
		}

		return mapped(got.logs[0].GetResourceLogs()[0].GetResource().GetAttributes())
	}

	if named := resource("one"); named["service.instance.id"] != "one" || named["host.name"] != nil {
		t.Errorf(
			"with an Instance: instance %v, host %v; want one, and no host",
			named["service.instance.id"],
			named["host.name"],
		)
	}

	if unnamed := resource(""); unnamed["service.instance.id"] != nil || text(unnamed["host.name"]) == "" {
		t.Errorf(
			"with no Instance: instance %v, host %v; want none, and the host's name",
			unnamed["service.instance.id"],
			unnamed["host.name"],
		)
	}
}

// TestAnInstrumentHasTwoThousandSeriesAtMost: what comes past them is
// counted in one more, marked as the overflow.
func TestAnInstrumentHasTwoThousandSeriesAtMost(t *testing.T) {
	t.Parallel()

	got := receive(t)

	config := got.config()
	config.Interval = time.Hour

	exporter := telemetry.New(config)
	requests := exporter.Counter("requests", "", "")

	const most, past = 2000, 100

	for user := range most + past {
		requests.With(slog.String("user", strconv.Itoa(user))).Add(1)
	}

	shutdown(t, exporter)

	points := got.lastMetrics()["requests"].GetSum().GetDataPoints()

	overflowed := int64(0)

	for _, point := range points {
		if mapped(point.GetAttributes())["otel.metric.overflow"] == true {
			overflowed += point.GetAsInt()
		}
	}

	if len(points) != most+1 || overflowed != past {
		t.Errorf("%d series, %d counted in the overflow; want %d and %d", len(points), overflowed, most+1, past)
	}
}

// TestBoundsThatRepeatAreAMistake: a histogram's bounds rise, each past the
// one before.
func TestBoundsThatRepeatAreAMistake(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("a histogram with the bound 1 twice did not panic")
		}
	}()

	telemetry.New(telemetry.Config{}).Histogram("spread", "", "", 1, 1)
}

// absent is a value whose methods fail on the pointer that is nil.
type absent struct{ name string }

func (a *absent) Error() string { return a.name }

type unnamed struct{ name string }

func (u *unnamed) String() string { return u.name }

// bursting is a value whose own method panics.
type bursting struct{}

func (bursting) String() string { panic("the value's own") }

// TestAValueWhoseMethodPanicsIsWrittenAsSlogWritesIt: a nil pointer of a
// type that is an error or a Stringer as "<nil>", and any other value whose
// method panics as the panic. Never as a panic in the program's own call: a
// log, a series' attributes, a resource's.
func TestAValueWhoseMethodPanicsIsWrittenAsSlogWritesIt(t *testing.T) {
	t.Parallel()

	got := receive(t)

	var (
		failed *absent
		who    *unnamed
	)

	config := got.config()
	config.Attributes = []slog.Attr{slog.Any("owner", who)}

	exporter := telemetry.New(config)

	slog.New(exporter.Handler(nil)).Error("failed", "err", failed, "who", who, "what", bursting{})
	exporter.Counter("requests", "", "").With(slog.Any("who", who), slog.Any("what", bursting{})).Add(1)

	shutdown(t, exporter)

	reported := got.records()
	if len(reported) != 1 {
		t.Fatalf("%d records received, want 1", len(reported))
	}

	attrs := mapped(reported[0].GetAttributes())

	if attrs["err"] != "<nil>" || attrs["who"] != "<nil>" || attrs["what"] != "!PANIC: the value's own" {
		t.Errorf("logged as %q, %q, %q", attrs["err"], attrs["who"], attrs["what"])
	}

	points := got.lastMetrics()["requests"].GetSum().GetDataPoints()
	if len(points) != 1 {
		t.Fatalf("%d series of the counter, want 1", len(points))
	}

	if series := mapped(
		points[0].GetAttributes(),
	); series["who"] != "<nil>" ||
		series["what"] != "!PANIC: the value's own" {
		t.Errorf("the series is of %q, %q", series["who"], series["what"])
	}

	got.mu.Lock()
	defer got.mu.Unlock()

	if owner := mapped(got.logs[0].GetResourceLogs()[0].GetResource().GetAttributes())["owner"]; owner != "<nil>" {
		t.Errorf("the resource's owner is %q", owner)
	}
}

// TestARoundOfLogsEndsWhateverIsLoggedMeanwhile: with a record logged
// during every request of logs, the metrics are still sent: a round sends
// what was waiting when it began, and the rest waits for the next.
func TestARoundOfLogsEndsWhateverIsLoggedMeanwhile(t *testing.T) {
	t.Parallel()

	got := receive(t)
	exporter := telemetry.New(got.config())
	logger := slog.New(exporter.Handler(nil))

	got.mu.Lock()
	got.onLogs = func() { logger.Info("logged while a request is out") }
	got.mu.Unlock()

	exporter.Counter("requests", "", "").Add(1)
	logger.Info("the first")

	for deadline := time.Now().Add(patient); len(got.lastMetrics()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no metrics were sent while a record was logged during every request of logs")
		}
	}

	got.mu.Lock()
	got.onLogs = nil
	got.mu.Unlock()

	shutdown(t, exporter)
}

// TestAnErrorDoesNotTellTheEndpointsPassword: what an endpoint's address
// holds of one is not in the error of a request it refused.
func TestAnErrorDoesNotTellTheEndpointsPassword(t *testing.T) {
	t.Parallel()

	got := receive(t)
	got.setAnswer(http.StatusBadRequest)

	told := make(chan error, 8)

	config := got.config()
	config.Endpoint = strings.Replace(got.URL, "http://", "http://sender:hunter2@", 1)
	config.OnError = func(err error) { told <- err }

	exporter := telemetry.New(config)

	slog.New(exporter.Handler(nil)).Info("a record")

	_ = exporter.Shutdown(t.Context())

	select {
	case err := <-told:
		if strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "sender") {
			t.Errorf("the error of a refused request: %v", err)
		}
	default:
		t.Error("a refused request was told to no one")
	}
}

// TestTheExporterReachesItsEndpointUnderItsTransport: the client is the
// exporter's own, made under Config.Transport. With none said, it is
// go-xhttp's baseline, which sends nothing in cleartext: an http endpoint
// gets no request, and the program is told why.
func TestTheExporterReachesItsEndpointUnderItsTransport(t *testing.T) {
	t.Parallel()

	got := receive(t)

	told := make(chan error, 8)

	config := got.config()
	config.Transport = xhttp.Config{}
	config.OnError = func(err error) { told <- err }

	exporter := telemetry.New(config)

	slog.New(exporter.Handler(nil)).Info("a record")

	_ = exporter.Shutdown(t.Context())

	select {
	case err := <-told:
		if !errors.Is(err, xhttp.ErrCleartext) || !errors.Is(err, telemetry.ErrExport) {
			t.Errorf("an http endpoint under the baseline: %v, want go-xhttp's refusal of cleartext", err)
		}
	default:
		t.Error("an http endpoint under the baseline: nothing was refused")
	}

	got.mu.Lock()
	defer got.mu.Unlock()

	if got.asked != 0 {
		t.Errorf("%d requests reached an http endpoint under the baseline, want none", got.asked)
	}
}
