package telemetry_test

// What holds the prototype to OTLP: every request it sends is decoded with
// OpenTelemetry's own generated types (go.opentelemetry.io/proto/otlp), and
// the handler is run through the standard library's own conformance test
// for slog handlers (testing/slogtest).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/slogtest"
	"time"

	logsservice "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricsservice "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	common "go.opentelemetry.io/proto/otlp/common/v1"
	logs "go.opentelemetry.io/proto/otlp/logs/v1"
	metrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/forkcloser/go-xhttp"

	"github.com/forkcloser/go-telemetry"
)

// receiver is a stand-in OTLP/HTTP endpoint that keeps what it is sent,
// decoded.
type receiver struct {
	*httptest.Server

	mu      sync.Mutex
	logs    []*logsservice.ExportLogsServiceRequest
	metrics []*metricsservice.ExportMetricsServiceRequest
	headers []http.Header
	// answer, when set, is the status of every answer; hold, when set, is
	// waited on before any.
	answer int
	hold   chan struct{}
	// asked is how many requests came, taken or not.
	asked int
	// onLogs, when set, is called for each request of logs before it is
	// answered.
	onLogs func()
	// complain is told what the stand-in finds wrong with a request: the
	// test's own Error, or a station's. Set once, before the server starts.
	complain func(string)
}

func receive(tb testing.TB) *receiver {
	tb.Helper()

	return stand(tb, httptest.NewServer, func(what string) { tb.Error(what) })
}

// stand starts the stand-in on the server start makes. complain is told
// what it finds wrong with a request, from the request's own goroutine.
func stand(tb testing.TB, start func(http.Handler) *httptest.Server, complain func(string)) *receiver {
	tb.Helper()

	got := &receiver{complain: complain}

	got.Server = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			got.say(err.Error())
		}

		got.mu.Lock()
		got.asked++
		hold, answer, onLogs := got.hold, got.answer, got.onLogs
		got.mu.Unlock()

		if onLogs != nil && r.URL.Path == "/v1/logs" {
			onLogs()
		}

		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}

		if answer != 0 {
			http.Error(w, "the stand-in refuses", answer)

			return
		}

		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-protobuf" {
			got.sayf("%s %s with Content-Type %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}

		got.mu.Lock()
		defer got.mu.Unlock()

		got.headers = append(got.headers, r.Header.Clone())

		switch r.URL.Path {
		case "/v1/logs":
			decoded := &logsservice.ExportLogsServiceRequest{}
			if err = proto.Unmarshal(body, decoded); err != nil {
				got.sayf("the logs request is not OTLP: %v", err)
			}

			got.logs = append(got.logs, decoded)
		case "/v1/metrics":
			decoded := &metricsservice.ExportMetricsServiceRequest{}
			if err = proto.Unmarshal(body, decoded); err != nil {
				got.sayf("the metrics request is not OTLP: %v", err)
			}

			got.metrics = append(got.metrics, decoded)
		default:
			got.sayf("a request for %s", r.URL.Path)
		}
	}))
	tb.Cleanup(got.Close)

	return got
}

// say has the stand-in complain of what it was sent. It takes no lock: the
// stand-in complains while it holds its own.
func (r *receiver) say(what string) { r.complain(what) }

func (r *receiver) sayf(format string, args ...any) { r.say(fmt.Sprintf(format, args...)) }

// config is a Config that reaches the receiver, and sends often.
func (r *receiver) config() telemetry.Config {
	return telemetry.Config{
		Endpoint:  r.URL,
		Transport: xhttp.Config{Cleartext: true},
		Service:   "prototype",
		Version:   "1.2.3",
		Instance:  "one",
		Interval:  20 * time.Millisecond,
	}
}

// records is every log record received, in order.
func (r *receiver) records() []*logs.LogRecord {
	r.mu.Lock()
	defer r.mu.Unlock()

	var all []*logs.LogRecord

	for _, request := range r.logs {
		for _, resource := range request.GetResourceLogs() {
			for _, scope := range resource.GetScopeLogs() {
				all = append(all, scope.GetLogRecords()...)
			}
		}
	}

	return all
}

// lastMetrics is the metrics of the last request received, by name.
func (r *receiver) lastMetrics() map[string]*metrics.Metric {
	r.mu.Lock()
	defer r.mu.Unlock()

	byName := map[string]*metrics.Metric{}

	if len(r.metrics) == 0 {
		return byName
	}

	for _, resource := range r.metrics[len(r.metrics)-1].GetResourceMetrics() {
		for _, scope := range resource.GetScopeMetrics() {
			for _, metric := range scope.GetMetrics() {
				byName[metric.GetName()] = metric
			}
		}
	}

	return byName
}

// plain turns an AnyValue into the Go value a test compares.
func plain(value *common.AnyValue) any {
	switch typed := value.GetValue().(type) {
	case *common.AnyValue_StringValue:
		return typed.StringValue
	case *common.AnyValue_IntValue:
		return typed.IntValue
	case *common.AnyValue_DoubleValue:
		return typed.DoubleValue
	case *common.AnyValue_BoolValue:
		return typed.BoolValue
	case *common.AnyValue_BytesValue:
		return typed.BytesValue
	case *common.AnyValue_KvlistValue:
		return mapped(typed.KvlistValue.GetValues())
	default:
		return nil
	}
}

// dropped is what the exporter last said it dropped.
func (r *receiver) dropped() int64 {
	count := int64(0)

	for _, point := range r.lastMetrics()["telemetry.log.dropped"].GetSum().GetDataPoints() {
		count += point.GetAsInt()
	}

	return count
}

// text, whole and group are a decoded value as what a test expects it to be,
// and the zero of that when it is anything else.
func text(value any) string {
	if is, ok := value.(string); ok {
		return is
	}

	return ""
}

func whole(value any) int64 {
	if is, ok := value.(int64); ok {
		return is
	}

	return 0
}

func group(value any) map[string]any {
	if is, ok := value.(map[string]any); ok {
		return is
	}

	return nil
}

// patient is how long a test waits for what is to happen before it calls it
// lost: far longer than any of it takes.
const patient = time.Minute

// errNoRoute is an error for a record to carry.
var errNoRoute = errors.New("no route")

func mapped(attrs []*common.KeyValue) map[string]any {
	into := map[string]any{}
	for _, attr := range attrs {
		into[attr.GetKey()] = plain(attr.GetValue())
	}

	return into
}

func shutdown(t *testing.T, exporter *telemetry.Exporter) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := exporter.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestALogRecordAsOTLP(t *testing.T) {
	t.Parallel()

	got := receive(t)
	exporter := telemetry.New(got.config())

	logger := slog.New(exporter.Handler(&telemetry.HandlerOptions{AddSource: true})).
		With("component", "door").WithGroup("request").With("id", 7)

	before := time.Now()

	logger.Warn("refused", "method", "GET", slog.Group("peer", "address", "192.0.2.7", "port", 51000),
		"elapsed", 1500*time.Millisecond, "ok", false, "ratio", 0.5, "cause", errNoRoute)
	logger.Debug("below the handler's level")

	shutdown(t, exporter)

	records := got.records()
	if len(records) != 1 {
		t.Fatalf("%d records received, want 1", len(records))
	}

	record := records[0]

	if at := time.Unix(0, int64(record.GetTimeUnixNano())); at.Before(before) || at.After(time.Now()) {
		t.Errorf("the record's time is %v", at)
	}

	if record.GetSeverityNumber() != logs.SeverityNumber_SEVERITY_NUMBER_WARN || record.GetSeverityText() != "WARN" {
		t.Errorf("severity %v %q, want WARN", record.GetSeverityNumber(), record.GetSeverityText())
	}

	if body := record.GetBody().GetStringValue(); body != "refused" {
		t.Errorf("body %q, want the message", body)
	}

	attrs := mapped(record.GetAttributes())

	if attrs["component"] != "door" {
		t.Errorf("component = %v, want the logger's own attribute", attrs["component"])
	}

	request := group(attrs["request"])
	peer := group(request["peer"])

	for key, want := range map[string]any{
		"id": int64(7), "method": "GET", "elapsed": int64(1500 * time.Millisecond), "ok": false, "ratio": 0.5, "cause": "no route",
	} {
		if request[key] != want {
			t.Errorf("request.%s = %v (%T), want %v", key, request[key], request[key], want)
		}
	}

	if peer["address"] != "192.0.2.7" || peer["port"] != int64(51000) {
		t.Errorf("request.peer = %v", peer)
	}

	if text(attrs["code.file.path"]) == "" || attrs["code.function.name"] == nil ||
		attrs["code.line.number"] == nil {
		t.Errorf("the source is missing: %v", attrs)
	}

	resource := mapped(got.logs[0].GetResourceLogs()[0].GetResource().GetAttributes())
	for key, want := range map[string]any{"service.name": "prototype", "service.version": "1.2.3", "service.instance.id": "one"} {
		if resource[key] != want {
			t.Errorf("resource %s = %v, want %v", key, resource[key], want)
		}
	}
}

// TestTheHandlerIsAnSlogHandler: the standard library's own test of what a
// handler owes slog, on the records as the receiver decoded them.
func TestTheHandlerIsAnSlogHandler(t *testing.T) {
	t.Parallel()

	got := receive(t)
	exporter := telemetry.New(got.config())

	err := slogtest.TestHandler(exporter.Handler(nil), func() []map[string]any {
		shutdown(t, exporter)

		var results []map[string]any

		for _, record := range got.records() {
			result := mapped(record.GetAttributes())

			if record.GetTimeUnixNano() != 0 {
				result[slog.TimeKey] = time.Unix(0, int64(record.GetTimeUnixNano()))
			}

			result[slog.LevelKey] = record.GetSeverityText()
			result[slog.MessageKey] = record.GetBody().GetStringValue()

			results = append(results, result)
		}

		return results
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMetricsAsOTLP(t *testing.T) {
	t.Parallel()

	got := receive(t)
	exporter := telemetry.New(got.config())

	requests := exporter.Counter("requests", "{request}", "Requests served.")
	requests.Add(2)
	requests.With(slog.String("method", "GET"), slog.Int("status", 200)).Add(3)
	// The same set in another order is the same series.
	requests.With(slog.Int("status", 200), slog.String("method", "GET")).Add(4)

	// A counter used through With alone sends no series of its own, and a
	// series With made is sent at zero.
	outcomes := exporter.Counter("outcomes", "{request}", "")
	outcomes.With(slog.String("outcome", "failed"))

	exporter.Gauge("queue.length", "{item}", "Items waiting.").Set(12.5)
	exporter.Gauge("never.set", "", "")

	took := exporter.Histogram("request.duration", "s", "How long a request took.", 0.1, 1, 10)
	for _, value := range []float64{0.05, 0.1, 0.5, 2, 20, 30} {
		took.Record(value)
	}

	shutdown(t, exporter)

	sent := got.lastMetrics()

	sum := sent["requests"].GetSum()
	if sum == nil || !sum.GetIsMonotonic() ||
		sum.GetAggregationTemporality() != metrics.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatalf("requests = %v, want a cumulative sum that only grows", sent["requests"])
	}

	counts := map[string]int64{}
	for _, point := range sum.GetDataPoints() {
		counts[fmt.Sprint(mapped(point.GetAttributes()))] = point.GetAsInt()

		if point.GetStartTimeUnixNano() == 0 || point.GetTimeUnixNano() < point.GetStartTimeUnixNano() {
			t.Errorf("a point from %d to %d", point.GetStartTimeUnixNano(), point.GetTimeUnixNano())
		}
	}

	if len(counts) != 2 {
		t.Fatalf("%d series of requests, want 2: %v", len(counts), counts)
	}

	for attrs, count := range counts {
		if want := map[string]int64{"map[]": 2, "map[method:GET status:200]": 7}[attrs]; count != want {
			t.Errorf("requests{%s} = %d, want %d", attrs, count, want)
		}
	}

	if sent["requests"].GetUnit() != "{request}" || sent["requests"].GetDescription() != "Requests served." {
		t.Errorf(
			"requests has unit %q and description %q",
			sent["requests"].GetUnit(),
			sent["requests"].GetDescription(),
		)
	}

	if points := sent["outcomes"].GetSum().GetDataPoints(); len(points) != 1 || points[0].GetAsInt() != 0 ||
		mapped(points[0].GetAttributes())["outcome"] != "failed" {
		t.Errorf("outcomes = %v, want the one series With made, at zero", points)
	}

	if points := sent["queue.length"].GetGauge().GetDataPoints(); len(points) != 1 || points[0].GetAsDouble() != 12.5 {
		t.Errorf("queue.length = %v, want one point of 12.5", points)
	}

	if _, sentUnset := sent["never.set"]; sentUnset {
		t.Error("a gauge never set was sent")
	}

	points := sent["request.duration"].GetHistogram().GetDataPoints()
	if len(points) != 1 {
		t.Fatalf("request.duration has %d points, want 1", len(points))
	}

	spread := points[0]

	// At or under 0.1: 0.05 and 0.1; under 1: 0.5; under 10: 2; past it: 20 and 30.
	if counts := spread.GetBucketCounts(); len(counts) != 4 || counts[0] != 2 || counts[1] != 1 || counts[2] != 1 ||
		counts[3] != 2 {
		t.Errorf("buckets %v, want [2 1 1 2]", counts)
	}

	if bounds := spread.GetExplicitBounds(); len(bounds) != 3 || bounds[0] != 0.1 || bounds[2] != 10 {
		t.Errorf("bounds %v", bounds)
	}

	if spread.GetCount() != 6 || spread.GetSum() != 52.65 || spread.GetMin() != 0.05 || spread.GetMax() != 30 {
		t.Errorf("count %d, sum %v, least %v, most %v; want 6, 52.65, 0.05, 30",
			spread.GetCount(), spread.GetSum(), spread.GetMin(), spread.GetMax())
	}
}

// TestLoggingNeverWaits: with an endpoint that answers nothing, ten thousand
// records are taken at once, the ones that do not fit are dropped, and the
// count of them is sent once the endpoint answers.
func TestLoggingNeverWaits(t *testing.T) {
	t.Parallel()

	got := receive(t)
	got.hold = make(chan struct{})

	config := got.config()
	config.Queue = 100

	exporter := telemetry.New(config)
	logger := slog.New(exporter.Handler(nil))

	const logged = 10000

	started := time.Now()

	for range logged {
		logger.Info("a record nobody is there to take")
	}

	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("%d records took %v with the endpoint silent", logged, took)
	}

	close(got.hold)
	shutdown(t, exporter)

	received := uint64(len(got.records()))
	dropped := uint64(0)

	for _, point := range got.lastMetrics()["telemetry.log.dropped"].GetSum().GetDataPoints() {
		dropped += uint64(point.GetAsInt())
	}

	if received == 0 || dropped == 0 || received+dropped != logged {
		t.Errorf("%d received and %d counted as dropped, of %d logged", received, dropped, logged)
	}
}

func TestARefusedBatchIsDroppedAndTold(t *testing.T) {
	t.Parallel()

	got := receive(t)
	got.answer = http.StatusBadRequest

	var (
		mu   sync.Mutex
		told []error
	)

	config := got.config()
	config.OnError = func(err error) {
		mu.Lock()
		defer mu.Unlock()

		told = append(told, err)
	}

	exporter := telemetry.New(config)
	slog.New(exporter.Handler(nil)).Info("refused")

	// A batch refused outright is not kept: nothing is left when it stops.
	shutdown(t, exporter)

	mu.Lock()
	defer mu.Unlock()

	if len(told) == 0 {
		t.Fatal("a refused batch was not told")
	}

	for _, err := range told {
		if !errors.Is(err, telemetry.ErrExport) {
			t.Errorf("%v is not ErrExport", err)
		}
	}
}

// waitAsked waits until the stand-in has been sent a request, taken or not.
func (r *receiver) waitAsked(t *testing.T) {
	t.Helper()

	for deadline := time.Now().Add(patient); ; time.Sleep(time.Millisecond) {
		r.mu.Lock()
		asked := r.asked
		r.mu.Unlock()

		if asked > 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatal("the stand-in was never sent a request")
		}
	}
}

// holding makes the stand-in keep every request waiting until the function
// it returns is called.
func (r *receiver) holding() (release func()) {
	hold := make(chan struct{})

	r.mu.Lock()
	r.hold = hold
	r.mu.Unlock()

	return sync.OnceFunc(func() { close(hold) })
}

// setAnswer makes the stand-in answer every request with status; zero is to
// take them.
func (r *receiver) setAnswer(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.answer = status
}

// TestWhatTheEndpointCannotTakeForNowWaits: records logged while the
// endpoint answers 503 are kept, in order, and arrive once it takes them
// again; past the queue's bound the oldest are dropped and counted.
func TestWhatTheEndpointCannotTakeForNowWaits(t *testing.T) {
	t.Parallel()

	got := receive(t)
	got.setAnswer(http.StatusServiceUnavailable)

	var (
		mu     sync.Mutex
		failed int
	)

	config := got.config()
	config.Queue = 10
	config.OnError = func(error) {
		mu.Lock()
		defer mu.Unlock()

		failed++
	}

	exporter := telemetry.New(config)
	logger := slog.New(exporter.Handler(nil))

	const logged = 25

	for index := range logged {
		logger.Info("while the endpoint is away", "index", index)
	}

	// Until the exporter has tried, and been refused, a few times: the
	// records are still its own.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		mu.Lock()
		tried := failed
		mu.Unlock()

		if tried >= 3 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the exporter never tried the endpoint")
		}
	}

	if received := len(got.records()); received != 0 {
		t.Fatalf("%d records received from an endpoint that answers 503", received)
	}

	got.setAnswer(0)
	shutdown(t, exporter)

	var indexes []int64

	for _, record := range got.records() {
		indexes = append(indexes, whole(mapped(record.GetAttributes())["index"]))
	}

	// The last ten, in the order they were logged.
	if len(indexes) != 10 || indexes[0] != logged-10 || indexes[9] != logged-1 || !slices.IsSorted(indexes) {
		t.Errorf("received the records %v, want the last 10 of %d in order", indexes, logged)
	}

	dropped := int64(0)
	for _, point := range got.lastMetrics()["telemetry.log.dropped"].GetSum().GetDataPoints() {
		dropped += point.GetAsInt()
	}

	if dropped != logged-10 {
		t.Errorf("%d records counted as dropped, want %d", dropped, logged-10)
	}
}

// TestStoppingWithTheEndpointAwaySaysWhatIsLost: Shutdown returns once it
// has tried, and its error counts the records left.
func TestStoppingWithTheEndpointAwaySaysWhatIsLost(t *testing.T) {
	t.Parallel()

	got := receive(t)
	got.setAnswer(http.StatusBadGateway)

	exporter := telemetry.New(got.config())
	logger := slog.New(exporter.Handler(nil))

	for range 3 {
		logger.Info("never sent")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := exporter.Shutdown(ctx)
	if err == nil || !errors.Is(err, telemetry.ErrExport) || !strings.Contains(err.Error(), "3 log records") {
		t.Errorf("Shutdown with the endpoint away = %v, want an ErrExport that counts 3 records", err)
	}
}

func TestBatchesAreBounded(t *testing.T) {
	t.Parallel()

	got := receive(t)
	exporter := telemetry.New(got.config())
	logger := slog.New(exporter.Handler(nil))

	const logged = 1500

	for range logged {
		logger.Info("one of many")
	}

	shutdown(t, exporter)

	if received := len(got.records()); received != logged {
		t.Fatalf("%d records received, want %d", received, logged)
	}

	got.mu.Lock()
	defer got.mu.Unlock()

	for _, request := range got.logs {
		if count := len(request.GetResourceLogs()[0].GetScopeLogs()[0].GetLogRecords()); count > 512 {
			t.Errorf("a request of %d records, want 512 at most", count)
		}
	}
}

// TestNoEndpointIsNothing: an application that logs and counts through an
// exporter with no endpoint runs as if there were none.
func TestNoEndpointIsNothing(t *testing.T) {
	t.Parallel()

	exporter := telemetry.New(telemetry.Config{})

	handler := exporter.Handler(nil)
	if handler.Enabled(t.Context(), slog.LevelError) {
		t.Error("the handler of an exporter with no endpoint is enabled")
	}

	slog.New(handler).With("key", "value").WithGroup("group").Error("goes nowhere")

	exporter.Counter("requests", "", "").With(slog.String("method", "GET")).Add(1)
	exporter.Gauge("level", "", "").With(slog.String("tank", "one")).Set(1)
	exporter.Histogram("spread", "", "", 1, 2).With(slog.String("route", "/")).Record(1)

	if err := exporter.Shutdown(t.Context()); err != nil {
		t.Error(err)
	}
}

func TestMistakesInTheProgramPanic(t *testing.T) {
	t.Parallel()

	got := receive(t)

	for name, mistake := range map[string]func(){
		"an endpoint that is no URL":  func() { telemetry.New(telemetry.Config{Endpoint: "::", Service: "prototype"}) },
		"an endpoint with no service": func() { telemetry.New(telemetry.Config{Endpoint: got.URL}) },
		"bounds that do not rise":     func() { telemetry.New(telemetry.Config{}).Histogram("spread", "", "", 2, 1) },
		"an http endpoint under a transport that sends nothing in cleartext": func() {
			telemetry.New(telemetry.Config{Endpoint: got.URL, Service: "prototype"})
		},
		"an endpoint under a transport that is offline": func() {
			telemetry.New(telemetry.Config{
				Endpoint: got.URL, Service: "prototype", Transport: xhttp.Config{Cleartext: true, Offline: true},
			})
		},
		"an endpoint that is neither http nor https": func() {
			telemetry.New(telemetry.Config{
				Endpoint: "ftp://collector.test", Service: "prototype", Transport: xhttp.Config{Cleartext: true},
			})
		},
		"one name for two sets of bounds": func() {
			exporter := telemetry.New(got.config())
			defer shutdown(t, exporter)

			exporter.Histogram("spread", "", "", 1, 2)
			exporter.Histogram("spread", "", "", 1, 3)
		},
		"one name for two kinds of them": func() {
			exporter := telemetry.New(got.config())
			defer shutdown(t, exporter)

			exporter.Counter("requests", "", "")
			exporter.Gauge("requests", "", "")
		},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()

			mistake()
		}()
	}
}

// TestAPanicDoesNotTellTheEndpointsPassword: every mistake in a Config that
// has an endpoint is told without what the endpoint holds of a password.
func TestAPanicDoesNotTellTheEndpointsPassword(t *testing.T) {
	t.Parallel()

	for name, config := range map[string]telemetry.Config{
		"an endpoint that does not parse": {Endpoint: "https://sender:hunter2 and more@collector.test", Service: "prototype"},
		"an endpoint with no host":        {Endpoint: "https://sender:hunter2@", Service: "prototype"},
		"an endpoint with no service":     {Endpoint: "https://sender:hunter2@collector.test"},
		"an endpoint in cleartext":        {Endpoint: "http://sender:hunter2@collector.test", Service: "prototype"},
		"an endpoint under a transport that is offline": {
			Endpoint: "https://sender:hunter2@collector.test", Service: "prototype", Transport: xhttp.Config{Offline: true},
		},
		"an endpoint that is neither http nor https": {Endpoint: "ftp://sender:hunter2@collector.test", Service: "prototype"},
	} {
		func() {
			defer func() {
				switch said := fmt.Sprint(recover()); {
				case said == "<nil>":
					t.Errorf("%s did not panic", name)
				case strings.Contains(said, "hunter2"):
					t.Errorf("%s: the panic tells the password: %s", name, said)
				}
			}()

			telemetry.New(config)
		}()
	}
}

// station is an exporter and the stand-in it reports to, for a test that
// sends through them many times over: a fuzzer runs thousands of inputs a
// second, and an exporter of its own for each would be a connection for
// each, more than a machine has ports for. One station is one connection.
type station struct {
	got      *receiver
	exporter *telemetry.Exporter

	// What the stand-in and the exporter complained of since the last input:
	// a fuzz target's own F may not be told while an input runs, so the
	// station keeps it for the T of the input that caused it.
	mu       sync.Mutex
	troubles []string
}

func (s *station) trouble(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.troubles = append(s.troubles, what)
}

// told reports to t what was complained of since the last call.
func (s *station) told(t *testing.T) {
	t.Helper()

	s.mu.Lock()
	troubles := s.troubles
	s.troubles = nil
	s.mu.Unlock()

	for _, what := range troubles {
		t.Error(what)
	}
}

// open is a station whose exporter sends what it holds every millisecond,
// stopped when the test ends.
func open(tb testing.TB) *station {
	tb.Helper()

	sends := &station{}
	sends.got = stand(tb, httptest.NewServer, sends.trouble)

	config := sends.got.config()
	config.Interval = time.Millisecond
	config.OnError = func(err error) { sends.trouble(err.Error()) }

	exporter := telemetry.New(config)
	sends.exporter = exporter

	tb.Cleanup(func() {
		//nolint:usetesting // the test's own context has ended by the time its cleanups run
		ctx, cancel := context.WithTimeout(context.Background(), patient)
		defer cancel()

		_ = exporter.Shutdown(ctx)
	})

	return sends
}

// next has send hand the exporter one record, and returns it as the
// stand-in received it. One at a time: what the stand-in holds is forgotten
// first, so that it holds what one test needs however many came before.
func (s *station) next(t *testing.T, send func(*telemetry.Exporter)) *logs.LogRecord {
	t.Helper()

	s.got.forget()
	send(s.exporter)

	defer s.told(t)

	for deadline := time.Now().Add(patient); ; time.Sleep(100 * time.Microsecond) {
		if reported := s.got.records(); len(reported) > 0 {
			return reported[0]
		}

		// A request the stand-in could not read, or an export that failed,
		// is why no record will come.
		s.mu.Lock()
		troubled := len(s.troubles) > 0
		s.mu.Unlock()

		if troubled || time.Now().After(deadline) {
			s.told(t)
			t.Fatal("the record never reached the stand-in")
		}
	}
}

// forget empties the stand-in of what it has received.
func (r *receiver) forget() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.logs, r.metrics, r.headers = nil, nil, nil
}

// FuzzALogRecord: whatever a program logs, bytes that are no text among it,
// is a request that decodes: the stand-in refuses one that does not.
func FuzzALogRecord(f *testing.F) {
	f.Add("a message", "key", "value")
	f.Add("", "", "")
	f.Add("\xff", "\xc3", "a\x80b")
	f.Add("€", "k\x00", strings.Repeat("\xf0\x9d", 3))

	sends := open(f)

	f.Fuzz(func(t *testing.T, message, key, value string) {
		// A record of more than 256 KiB is dropped, which another test is
		// of, and each byte that is no text is written as three.
		if len(message)+len(key)+len(value) > 64<<10 {
			t.Skip("more than a record carries")
		}

		record := sends.next(t, func(exporter *telemetry.Exporter) {
			logger := slog.New(exporter.Handler(nil))

			logger.Info(message, slog.String(key, value)) //nolint:sloglint // the message is what is fuzzed
		})

		if body := record.GetBody().GetStringValue(); body != strings.ToValidUTF8(message, "�") {
			t.Errorf("the body is %q, of the message %q", body, message)
		}

		if key == "" {
			return
		}

		attrs := mapped(record.GetAttributes())

		if stored := attrs[strings.ToValidUTF8(key, "�")]; stored != strings.ToValidUTF8(value, "�") {
			t.Errorf("the attribute %q is %q, of the value %q", key, stored, value)
		}
	})
}
