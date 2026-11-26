package telemetry

import (
	"context"
	"log/slog"
	"runtime"
	"slices"
	"time"
)

// The fields of OTLP's LogRecord (opentelemetry/proto/logs/v1/logs.proto).
const (
	recordTime         = 1
	recordSeverity     = 2
	recordSeverityText = 3
	recordBody         = 5
	recordAttributes   = 6
	recordObservedTime = 11
)

// HandlerOptions is what varies between an exporter's handlers.
type HandlerOptions struct {
	// Level is the least level exported; slog.LevelInfo with none.
	Level slog.Leveler
	// AddSource adds where the record was logged from, as code.file.path,
	// code.line.number and code.function.name.
	AddSource bool
}

// Handler returns a slog.Handler that exports each record it is given as an
// OTLP log record: the message as its body, the level as its severity, the
// attributes as its attributes, a group as a map of them. It is one handler
// among an application's: slog.NewMultiHandler puts it beside the one that
// writes to the terminal.
//
// Handle never waits on the endpoint. With no endpoint the handler is never
// enabled.
func (e *Exporter) Handler(options *HandlerOptions) slog.Handler {
	if options == nil {
		options = &HandlerOptions{}
	}

	level := options.Level
	if level == nil {
		level = slog.LevelInfo
	}

	return &handler{sender: e.sender, level: level, source: options.AddSource, frames: []frame{{}}}
}

// frame is the attributes WithAttrs gave at one depth of WithGroup, already
// written: the first frame's as the record's own attributes, any other's as
// the members of its group.
type frame struct {
	group string
	attrs []byte
}

type handler struct {
	sender *sender
	level  slog.Leveler
	source bool
	// frames is never empty, and never shared: a With copies it.
	frames []frame
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return h.sender != nil && level >= h.level.Level()
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}

	with := *h
	with.frames = slices.Clone(h.frames)

	last := &with.frames[len(with.frames)-1]
	last.attrs = slices.Clone(last.attrs)

	written := message(last.attrs)

	for _, attr := range attrs {
		appendAttr(&written, with.field(), attr)
	}

	last.attrs = written

	return &with
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}

	with := *h
	with.frames = append(slices.Clone(h.frames), frame{group: name})

	return &with
}

func (h *handler) Handle(_ context.Context, record slog.Record) error {
	if h.sender == nil {
		return nil
	}

	written := message{}

	if !record.Time.IsZero() {
		written.fixed(recordTime, nanoseconds(record.Time))
	}

	written.fixed(recordObservedTime, nanoseconds(time.Now()))
	written.uint(recordSeverity, severity(record.Level))
	written.text(recordSeverityText, record.Level.String())
	written.nested(recordBody, stringValue(record.Message))

	// The record's own attributes belong to the innermost group.
	innermost := message(slices.Clone(h.frames[len(h.frames)-1].attrs))

	record.Attrs(func(attr slog.Attr) bool {
		appendAttr(&innermost, h.field(), attr)

		return true
	})

	// Each group is closed into the one around it, from the innermost out.
	// A group with nothing in it is left out, as slog asks.
	for depth := len(h.frames) - 1; depth > 0; depth-- {
		outer := message(slices.Clone(h.frames[depth-1].attrs))

		if len(innermost) > 0 {
			field := listValues
			if depth == 1 {
				field = recordAttributes
			}

			list := message{}

			list.nested(anyKVList, innermost)
			outer.nested(field, keyValue(h.frames[depth].group, list))
		}

		innermost = outer
	}

	written = append(written, innermost...)

	if h.source && record.PC != 0 {
		frames := runtime.CallersFrames([]uintptr{record.PC})
		from, _ := frames.Next()

		appendAttr(&written, recordAttributes, slog.String("code.file.path", from.File))
		appendAttr(&written, recordAttributes, slog.Int("code.line.number", from.Line))
		appendAttr(&written, recordAttributes, slog.String("code.function.name", from.Function))
	}

	h.sender.enqueue(written)

	return nil
}

// field is the field the attributes of the last frame are written under: a
// record's attributes, or a list's values inside a group.
func (h *handler) field() int {
	if len(h.frames) == 1 {
		return recordAttributes
	}

	return listValues
}

// severity is OTLP's severity number for a slog level: INFO is 9 where slog's
// is 0, and both step by four from one name to the next. The numbers run
// from 1 to 24.
func severity(level slog.Level) uint64 {
	const info, least, most = 9, 1, 24

	return uint64(min(max(int(level)+info, least), most))
}

func nanoseconds(at time.Time) uint64 {
	return uint64(max(at.UnixNano(), 0))
}
