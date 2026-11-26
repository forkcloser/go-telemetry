package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/forkcloser/go-xhttp"
)

const (
	// batchRecords and batchBytes bound one request of logs. A request the
	// house's collector takes is 16 MiB at most.
	batchRecords = 512
	batchBytes   = 4 << 20
	// recordBytes bounds one log record: a larger one is dropped and counted.
	recordBytes = 256 << 10
	// queueBytes bounds what the queue holds, whatever its count of records.
	queueBytes = 16 << 20
	// exportTimeout bounds one request.
	exportTimeout = 30 * time.Second

	scopeName = "github.com/forkcloser/go-telemetry"
)

// The fields of OTLP's envelopes: the export requests, ResourceLogs and
// ResourceMetrics, ScopeLogs and ScopeMetrics, Resource and
// InstrumentationScope.
const (
	requestResources = 1

	resourceEnvelopeResource = 1
	resourceEnvelopeScopes   = 2

	scopeEnvelopeScope = 1
	scopeEnvelopeItems = 2

	resourceAttributes = 1

	scopeFieldName = 1
)

// Config is what varies between exporters. It is the program's to fill, from
// wherever the program has its configuration: this package reads none of
// its own, and none from the environment.
type Config struct {
	// Endpoint is the base URL of an OTLP/HTTP receiver: logs go to its
	// /v1/logs and metrics to its /v1/metrics. With none, the exporter
	// exports nothing: its handler is never enabled, its instruments count
	// nothing, and nothing runs.
	Endpoint string
	// Headers are added to every request.
	Headers http.Header
	// Transport is how the endpoint is reached: the exporter makes its own
	// go-xhttp client under it, and uses no other. Its zero value is
	// go-xhttp's baseline, https with a post-quantum hybrid key exchange
	// only; an endpoint that needs less, an http one among them, is one the
	// program says so of here. A program hands the exporter no client of its
	// own and shares no connection with it.
	Transport xhttp.Config
	// Service, Version and Instance are the resource's service.name,
	// service.version and service.instance.id. With no Instance, host.name
	// is the host's name.
	Service  string
	Version  string
	Instance string
	// Attributes are the resource's other attributes.
	Attributes []slog.Attr
	// Interval is how often the exporter sends what it holds; zero is
	// DefaultInterval.
	Interval time.Duration
	// Queue is how many log records the exporter holds at most, between two
	// sends and while the endpoint is out of reach. Past it the oldest are
	// dropped and counted. Zero is DefaultQueue. The queue is 16 MiB at most
	// whatever its count.
	Queue int
	// OnError is told each failed export. It must not log through this
	// exporter's own handler.
	OnError func(error)
	// CrashDelivery is how long a watcher ([Watch]) may take to deliver the
	// report of a crash, which is how long it goes on after the program when
	// the endpoint is out of reach; zero is DefaultCrashDelivery.
	CrashDelivery time.Duration
}

// Exporter sends an application's log records and metrics to an OTLP/HTTP
// endpoint. It never makes the application wait. Records the endpoint cannot
// take for now, because it is out of reach or answers 429, 502, 503 or 504,
// wait in a queue in memory and are sent again at the next interval; when
// the queue is full its oldest records are dropped and counted. A batch the
// endpoint refuses outright is dropped.
type Exporter struct {
	// Nil for an exporter with no endpoint, which does nothing.
	sender *sender
}

// sender is what an exporter with an endpoint is made of.
type sender struct {
	logsURL    string
	metricsURL string
	headers    http.Header
	client     *http.Client
	resource   []byte // a Resource message
	interval   time.Duration
	onError    func(error)
	started    time.Time

	// The log records waiting to be sent, each a LogRecord message, oldest
	// first, and their bound.
	queueMu     sync.Mutex
	queue       [][]byte
	queuedBytes int
	queueSize   int

	full   chan struct{} // told when the queue holds a batch
	stop   chan struct{}
	done   chan struct{}
	closed atomic.Bool
	once   sync.Once

	mu          sync.Mutex
	instruments []instrument
	byName      map[string]instrument

	// What the exporter has to say about itself, sent as metrics.
	droppedRecords *Counter
	failedExports  *Counter
}

// New returns an exporter under config, and starts it. Shutdown stops it.
// It panics on an endpoint that is no URL, on an endpoint with no Service: a
// record nobody can attribute is worth nothing; and, as go-xhttp does, on a
// Transport that is wrong.
func New(config Config) *Exporter {
	if config.Endpoint == "" {
		return &Exporter{}
	}

	base, client := config.reached()

	interval := config.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	size := config.Queue
	if size <= 0 {
		size = DefaultQueue
	}

	made := &sender{
		logsURL:    base.JoinPath("v1", "logs").String(),
		metricsURL: base.JoinPath("v1", "metrics").String(),
		headers:    config.Headers,
		client:     client,
		resource:   resource(config),
		interval:   interval,
		onError:    config.OnError,
		started:    time.Now(),
		queueSize:  size,
		full:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		byName:     map[string]instrument{},
	}

	exporter := &Exporter{sender: made}

	made.droppedRecords = exporter.Counter(
		"telemetry.log.dropped",
		"{record}",
		"Log records dropped before they were sent: the queue was full, the record too large, the endpoint refused them, or the exporter stopped with the endpoint out of reach.",
	).With()
	made.failedExports = exporter.Counter("telemetry.export.failed", "{request}",
		"Export requests the endpoint did not take.").With()

	go made.run()

	return exporter
}

// reached is the endpoint of a Config that has one, and a client of its own
// that reaches it. It panics as New says.
func (config Config) reached() (*url.URL, *http.Client) {
	base, err := url.Parse(config.Endpoint)
	if err != nil || base.Host == "" {
		panic(fmt.Sprintf("telemetry: the endpoint %q is no URL", config.Endpoint))
	}

	if config.Service == "" {
		panic("telemetry: an exporter with an endpoint needs a Service")
	}

	return base, xhttp.NewClient(config.Transport)
}

// resource writes the Resource message of every request.
func resource(config Config) []byte {
	attrs := []slog.Attr{slog.String("service.name", config.Service)}

	if config.Version != "" {
		attrs = append(attrs, slog.String("service.version", config.Version))
	}

	if config.Instance != "" {
		attrs = append(attrs, slog.String("service.instance.id", config.Instance))
	} else if host, err := os.Hostname(); err == nil {
		attrs = append(attrs, slog.String("host.name", host))
	}

	written := message{}

	for _, attr := range append(attrs, config.Attributes...) {
		appendAttr(&written, resourceAttributes, attr)
	}

	return written
}

// Shutdown sends what the exporter still holds and stops it, within ctx.
// What is logged or counted after it goes nowhere. It may be called twice.
// Records the endpoint could not take by then are lost, and the error, an
// [ErrExport], says how many.
func (e *Exporter) Shutdown(ctx context.Context) error {
	if e.sender == nil {
		return nil
	}

	made := e.sender

	made.once.Do(func() {
		made.closed.Store(true)
		close(made.stop)
	})

	select {
	case <-made.done:
	case <-ctx.Done():
		return fmt.Errorf("telemetry: shutdown: %w", context.Cause(ctx))
	}

	made.queueMu.Lock()
	lost := len(made.queue)
	made.queueMu.Unlock()

	if lost > 0 {
		return fmt.Errorf("%w: %d log records were still waiting when the exporter stopped", ErrExport, lost)
	}

	return nil
}

// run sends at every interval and whenever a batch of records is waiting,
// until told to stop, and once more then.
func (s *sender) run() {
	defer close(s.done)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-s.full:
			s.sendLogs()
		case <-ticker.C:
			s.sendLogs()
			s.sendMetrics()
		case <-s.stop:
			s.sendLogs()
			s.sendMetrics()

			return
		}
	}
}

// enqueue takes a written log record. It never waits: when the queue is
// past its bound the oldest records make room, dropped and counted, so that
// what an endpoint finds when it comes back is the latest.
func (s *sender) enqueue(record []byte) {
	if s.closed.Load() || len(record) > recordBytes {
		s.droppedRecords.Add(1)

		return
	}

	s.queueMu.Lock()
	s.queue = append(s.queue, record)
	s.queuedBytes += len(record)
	waiting := s.trim()
	s.queueMu.Unlock()

	if waiting >= batchRecords {
		select {
		case s.full <- struct{}{}:
		default:
		}
	}
}

// trim drops the oldest records while the queue is past its bound, counts
// them, and returns how many records are left. The caller holds the lock.
func (s *sender) trim() int {
	dropped := 0

	for len(s.queue) > s.queueSize || s.queuedBytes > queueBytes {
		s.queuedBytes -= len(s.queue[0])
		s.queue[0] = nil
		s.queue = s.queue[1:]
		dropped++
	}

	if dropped > 0 {
		s.droppedRecords.Add(uint64(dropped))
	}

	return len(s.queue)
}

// take removes the oldest records from the queue: most of them at most, and
// no more than a batch holds, by count and by size.
func (s *sender) take(most int) [][]byte {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	count, size := 0, 0

	for count < len(s.queue) && count < min(most, batchRecords) {
		// A record is smaller than a batch, so the first one always fits.
		if count > 0 && size+len(s.queue[count]) > batchBytes {
			break
		}

		size += len(s.queue[count])
		count++
	}

	batch := s.queue[:count:count]
	s.queue = s.queue[count:]
	s.queuedBytes -= size

	return batch
}

// putBack returns a batch the endpoint could not take for now to the front
// of the queue, where it came from, within the queue's bound.
func (s *sender) putBack(batch [][]byte) {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()

	s.queue = append(batch, s.queue...)

	for _, record := range batch {
		s.queuedBytes += len(record)
	}

	s.trim()
}

// sendLogs sends the records that are waiting when it is called, a batch a
// request, until none of them is left or the endpoint cannot take one for
// now: that batch goes back to the queue and waits for the next round. A
// batch the endpoint refuses outright is dropped: sending it again would be
// refused again.
//
// What is logged while a round is at it waits for the next: a round that
// went on for as long as records came would never end under a program that
// logs steadily, and the metrics wait behind it.
func (s *sender) sendLogs() {
	s.queueMu.Lock()
	waiting := len(s.queue)
	s.queueMu.Unlock()

	for waiting > 0 {
		batch := s.take(waiting)
		if len(batch) == 0 {
			return
		}

		waiting -= len(batch)

		records := message{}

		for _, record := range batch {
			records.nested(scopeEnvelopeItems, record)
		}

		later, err := s.post(context.Background(), s.logsURL, envelope(s.resource, records))
		if err == nil {
			continue
		}

		s.failed(err)

		if later {
			s.putBack(batch)

			return
		}

		s.droppedRecords.Add(uint64(len(batch)))
	}
}

// sendMetrics sends every instrument's series as they stand.
func (s *sender) sendMetrics() {
	s.mu.Lock()
	instruments := append([]instrument(nil), s.instruments...)
	s.mu.Unlock()

	metrics := message{}

	now := time.Now()

	for _, each := range instruments {
		if metric := each.metric(s.started, now); metric != nil {
			metrics.nested(scopeEnvelopeItems, metric)
		}
	}

	if len(metrics) == 0 {
		return
	}

	// A failed request loses nothing: the next one carries every sum again.
	if _, err := s.post(context.Background(), s.metricsURL, envelope(s.resource, metrics)); err != nil {
		s.failed(err)
	}
}

// envelope wraps the items of one scope, log records or metrics already
// written under their field, into an export request for one resource.
func envelope(resource []byte, items message) []byte {
	scope, scoped, resourced, request := message{}, message{}, message{}, message{}

	scope.text(scopeFieldName, scopeName)

	scoped.nested(scopeEnvelopeScope, scope)
	scoped = append(scoped, items...)

	resourced.nested(resourceEnvelopeResource, resource)
	resourced.nested(resourceEnvelopeScopes, scoped)

	request.nested(requestResources, resourced)

	return request
}

func (s *sender) failed(err error) {
	s.failedExports.Add(1)

	if s.onError != nil {
		s.onError(err)
	}
}

// post sends one request. Anything but a 2xx is an error, and later says
// whether the same request may be taken another time: the endpoint was not
// reached, or answered 429, 502, 503 or 504, the statuses OTLP names as worth
// sending again.
func (s *sender) post(ctx context.Context, address string, body []byte) (later bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, exportTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrExport, err)
	}

	maps.Copy(req.Header, s.headers)

	req.Header.Set("Content-Type", "application/x-protobuf")

	resp, err := s.client.Do(req)
	if err != nil {
		return true, fmt.Errorf("%w: %w", ErrExport, err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return false, nil
	}

	const shown = 512

	said, _ := io.ReadAll(io.LimitReader(resp.Body, shown))
	// Without what the address may hold of a password, as the standard
	// library's own errors name it.
	err = fmt.Errorf("%w: %s answered %s: %s", ErrExport, req.URL.Redacted(), resp.Status, bytes.TrimSpace(said))

	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true, err
	default:
		return false, err
	}
}
