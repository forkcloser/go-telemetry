package telemetry

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// The fields of OTLP's metrics messages
// (opentelemetry/proto/metrics/v1/metrics.proto).
const (
	metricName        = 1
	metricDescription = 2
	metricUnit        = 3
	metricGauge       = 5
	metricSum         = 7
	metricHistogram   = 9

	dataPoints      = 1
	temporality     = 2
	sumIsMonotonic  = 3
	cumulative      = 2
	pointStart      = 2
	pointTime       = 3
	numberAsDouble  = 4
	numberAsInt     = 6
	numberAttrs     = 7
	histogramCount  = 4
	histogramSum    = 5
	histogramCounts = 6
	histogramBounds = 7
	histogramAttrs  = 9
	histogramMin    = 11
	histogramMax    = 12
)

// maxSeries bounds the series of one instrument: an attribute whose values
// come from input would otherwise grow the exporter, and the store, without
// end. Past it, a new set of attributes is counted in one series marked
// otel.metric.overflow, as OpenTelemetry's SDKs do.
const maxSeries = 2000

// instrument is what the exporter asks of each of its instruments at an
// export: its Metric message as it stands, or nil when it has nothing yet.
type instrument interface {
	metric(started, now time.Time) []byte
}

// family is one instrument: its name, and one series for each set of
// attributes it was given.
type family[S any] struct {
	name, unit, description string
	attrField               int
	create                  func() *S

	mu     sync.Mutex
	series map[string]*S // by the attributes, as they are written
	keys   []string      // in the order the series were made
}

// at is the family's series for attrs, made when first asked for.
func (f *family[S]) at(attrs []slog.Attr) *S {
	key := string(attributes(f.attrField, attrs))

	f.mu.Lock()
	defer f.mu.Unlock()

	if found, ok := f.series[key]; ok {
		return found
	}

	if len(f.series) >= maxSeries {
		key = string(attributes(f.attrField, []slog.Attr{slog.Bool("otel.metric.overflow", true)}))

		if found, ok := f.series[key]; ok {
			return found
		}
	}

	made := f.create()
	f.series[key] = made
	f.keys = append(f.keys, key)

	return made
}

// each calls point for every series, in the order they were made.
func (f *family[S]) each(point func(attrs string, series *S)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, key := range f.keys {
		point(key, f.series[key])
	}
}

// header starts the family's Metric message.
func (f *family[S]) header() message {
	written := message{}

	written.text(metricName, f.name)
	written.text(metricDescription, f.description)
	written.text(metricUnit, f.unit)

	return written
}

// attributes writes a series' attributes under field, sorted by key so that
// one set has one writing whatever order it was given in. A group's members
// are written under its name and a dot.
func attributes(field int, attrs []slog.Attr) []byte {
	flat := flatten(nil, "", attrs)

	slices.SortStableFunc(flat, func(one, other slog.Attr) int { return cmp.Compare(one.Key, other.Key) })

	written := message{}

	for _, attr := range flat {
		written.nested(field, keyValue(attr.Key, anyValue(attr.Value)))
	}

	return written
}

func flatten(into []slog.Attr, prefix string, attrs []slog.Attr) []slog.Attr {
	for _, attr := range attrs {
		attr.Value = attr.Value.Resolve()

		switch {
		case attr.Equal(slog.Attr{}):
		case attr.Value.Kind() == slog.KindGroup && attr.Key == "":
			into = flatten(into, prefix, attr.Value.Group())
		case attr.Value.Kind() == slog.KindGroup:
			into = flatten(into, prefix+attr.Key+".", attr.Value.Group())
		default:
			into = append(into, slog.Attr{Key: prefix + attr.Key, Value: attr.Value})
		}
	}

	return into
}

// register gives the exporter's instrument of that name, made when first
// asked for. It panics when the name is another kind of instrument's: two
// meanings under one name is a mistake in the program.
func register[I instrument](into *sender, name string, create func() I) I {
	into.mu.Lock()
	defer into.mu.Unlock()

	if existing, ok := into.byName[name]; ok {
		same, isSame := existing.(I)
		if !isSame {
			panic(fmt.Sprintf("telemetry: the instrument %q is already a %T", name, existing))
		}

		return same
	}

	made := create()
	into.byName[name] = made
	into.instruments = append(into.instruments, made)

	return made
}

func newFamily[S any](name, unit, description string, attrField int, create func() *S) *family[S] {
	return &family[S]{
		name: name, unit: unit, description: description,
		attrField: attrField, create: create, series: map[string]*S{},
	}
}

// point starts a data point: its attributes, already written, and its times.
func point(attrs string, started, now time.Time) message {
	written := message(attrs)

	written.fixed(pointStart, nanoseconds(started))
	written.fixed(pointTime, nanoseconds(now))

	return written
}

// counterFamily is a sum that only grows.
type counterFamily struct{ *family[atomic.Uint64] }

func (c counterFamily) metric(started, now time.Time) []byte {
	sum := message{}

	c.each(func(attrs string, series *atomic.Uint64) {
		written := point(attrs, started, now)
		// OTLP's integer is signed: a count past its range stays at its end.
		written.fixed(numberAsInt, min(series.Load(), math.MaxInt64))

		sum.nested(dataPoints, written)
	})

	if len(sum) == 0 {
		return nil
	}

	sum.uint(temporality, cumulative)
	sum.uint(sumIsMonotonic, 1)

	metric := c.header()
	metric.nested(metricSum, sum)

	return metric
}

// Counter counts what only grows: requests served, bytes sent, errors met.
// The exporter sends its total since the exporter started.
type Counter struct {
	// Nil for the counter of an exporter with no endpoint.
	family *family[atomic.Uint64]
	// The series With gave, there from the start at zero; nil for the
	// counter itself, whose series with no attribute is made at its first
	// Add, so that a counter only ever used through With sends no empty one.
	series *atomic.Uint64
	first  sync.Once
}

// Counter returns the exporter's counter of that name, made when first asked
// for. The name and the unit follow OpenTelemetry's conventions: "requests"
// counted in "{request}", sizes in "By", times in "s". It panics when the
// name is already an instrument of another kind.
func (e *Exporter) Counter(name, unit, description string) *Counter {
	if e.sender == nil {
		return &Counter{}
	}

	made := register(e.sender, name, func() counterFamily {
		return counterFamily{
			newFamily(name, unit, description, numberAttrs, func() *atomic.Uint64 { return new(atomic.Uint64) }),
		}
	})

	return &Counter{family: made.family}
}

// With returns the counter's series for attrs, as a logger's With returns a
// logger. The series is sent from then on, at zero until it counts, so that
// a rate over it starts where it should. Keep what With returns where it is
// used often: Add on it is one atomic addition.
func (c *Counter) With(attrs ...slog.Attr) *Counter {
	if c.family == nil {
		return c
	}

	return &Counter{family: c.family, series: c.family.at(attrs)}
}

// Add adds to the count.
func (c *Counter) Add(count uint64) {
	if c.family == nil {
		return
	}

	c.first.Do(func() {
		if c.series == nil {
			c.series = c.family.at(nil)
		}
	})

	c.series.Add(count)
}

// level is the last value a gauge was set to, and whether it ever was.
type level struct {
	bits atomic.Uint64
	set  atomic.Bool
}

type gaugeFamily struct{ *family[level] }

func (g gaugeFamily) metric(_, now time.Time) []byte {
	gauge := message{}

	g.each(func(attrs string, series *level) {
		if !series.set.Load() {
			return
		}

		written := message(attrs)
		written.fixed(pointTime, nanoseconds(now))
		written.fixed(numberAsDouble, series.bits.Load())

		gauge.nested(dataPoints, written)
	})

	if len(gauge) == 0 {
		return nil
	}

	metric := g.header()
	metric.nested(metricGauge, gauge)

	return metric
}

// Gauge holds what goes up and down: a queue's length, a temperature, the
// connections open. The exporter sends the value it was last set to, and
// nothing for a gauge never set.
type Gauge struct {
	family *family[level]
	series *level
}

// Gauge returns the exporter's gauge of that name, as [Exporter.Counter]
// returns a counter.
func (e *Exporter) Gauge(name, unit, description string) *Gauge {
	if e.sender == nil {
		return &Gauge{}
	}

	made := register(e.sender, name, func() gaugeFamily {
		return gaugeFamily{newFamily(name, unit, description, numberAttrs, func() *level { return new(level) })}
	})

	return &Gauge{family: made.family, series: made.at(nil)}
}

// With returns the gauge's series for attrs.
func (g *Gauge) With(attrs ...slog.Attr) *Gauge {
	if g.family == nil {
		return g
	}

	return &Gauge{family: g.family, series: g.family.at(attrs)}
}

// Set sets the gauge.
func (g *Gauge) Set(value float64) {
	if g.series != nil {
		g.series.bits.Store(math.Float64bits(value))
		g.series.set.Store(true)
	}
}

// distribution is what a histogram has seen: how many values fell at or
// under each bound and past the last, and their count, sum, least and most.
type distribution struct {
	mu          sync.Mutex
	bounds      []float64
	counts      []uint64
	count       uint64
	sum         float64
	least, most float64
}

type histogramFamily struct {
	*family[distribution]

	// bounds is the upper edges of every series' buckets.
	bounds []float64
}

func (h histogramFamily) metric(started, now time.Time) []byte {
	histogram := message{}

	h.each(func(attrs string, series *distribution) {
		series.mu.Lock()
		defer series.mu.Unlock()

		if series.count == 0 {
			return
		}

		written := point(attrs, started, now)
		written.fixed(histogramCount, series.count)
		written.double(histogramSum, series.sum)
		written.nested(histogramCounts, packed(series.counts, func(count uint64) uint64 { return count }))
		written.nested(histogramBounds, packed(series.bounds, math.Float64bits))
		written.double(histogramMin, series.least)
		written.double(histogramMax, series.most)

		histogram.nested(dataPoints, written)
	})

	if len(histogram) == 0 {
		return nil
	}

	histogram.uint(temporality, cumulative)

	metric := h.header()
	metric.nested(metricHistogram, histogram)

	return metric
}

// packed writes numbers as protobuf packs a repeated fixed64 field.
func packed[N any](values []N, bits func(N) uint64) []byte {
	written := make([]byte, 0, len(values)*binary.MaxVarintLen64)

	for _, value := range values {
		written = binary.LittleEndian.AppendUint64(written, bits(value))
	}

	return written
}

// Histogram holds how values are spread: how long requests took, how large
// bodies were. The exporter sends, since it started, how many values fell at
// or under each bound, and their count, sum, least and most.
type Histogram struct {
	family *family[distribution]
	series *distribution
}

// Histogram returns the exporter's histogram of that name, as
// [Exporter.Counter] returns a counter. The bounds are the upper edges of
// its buckets, each greater than the one before; values past the last are
// counted in one more bucket. It panics on bounds that are not rising, or
// not numbers.
func (e *Exporter) Histogram(name, unit, description string, bounds ...float64) *Histogram {
	for index, bound := range bounds {
		if math.IsNaN(bound) || math.IsInf(bound, 0) || index > 0 && bound <= bounds[index-1] {
			panic(fmt.Sprintf("telemetry: the bounds of the histogram %q do not rise: %v", name, bounds))
		}
	}

	if e.sender == nil {
		return &Histogram{}
	}

	made := register(e.sender, name, func() histogramFamily {
		own := slices.Clone(bounds)

		return histogramFamily{family: newFamily(name, unit, description, histogramAttrs, func() *distribution {
			return &distribution{bounds: own, counts: make([]uint64, len(own)+1)}
		}), bounds: own}
	})

	if !slices.Equal(made.bounds, bounds) {
		panic(fmt.Sprintf("telemetry: the histogram %q has the bounds %v already, not %v", name, made.bounds, bounds))
	}

	return &Histogram{family: made.family, series: made.at(nil)}
}

// With returns the histogram's series for attrs.
func (h *Histogram) With(attrs ...slog.Attr) *Histogram {
	if h.family == nil {
		return h
	}

	return &Histogram{family: h.family, series: h.family.at(attrs)}
}

// Record adds one value. One that is not a number is left out.
func (h *Histogram) Record(value float64) {
	if h.series == nil || math.IsNaN(value) {
		return
	}

	series := h.series

	series.mu.Lock()
	defer series.mu.Unlock()

	// The first bound the value is not past: OTLP's buckets hold their upper
	// edge.
	bucket, _ := slices.BinarySearch(series.bounds, value)
	series.counts[bucket]++

	if series.count == 0 || value < series.least {
		series.least = value
	}

	if series.count == 0 || value > series.most {
		series.most = value
	}

	series.count++
	series.sum += value
}
