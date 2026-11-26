package telemetry

import (
	"encoding"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// The fields of OTLP's common messages
// (opentelemetry/proto/common/v1/common.proto).
const (
	anyString = 1
	anyBool   = 2
	anyInt    = 3
	anyDouble = 4
	anyArray  = 5
	anyKVList = 6
	anyBytes  = 7

	keyValueKey   = 1
	keyValueValue = 2

	listValues = 1
)

// keyValue writes one KeyValue message, the body of a repeated field.
func keyValue(key string, value []byte) []byte {
	written := message{}

	written.text(keyValueKey, key)
	written.nested(keyValueValue, value)

	return written
}

// stringValue is an AnyValue holding a string. A value is one of several
// kinds, so its field is written even when empty: an absent one is no value.
func stringValue(value string) []byte {
	written := message{}

	value = strings.ToValidUTF8(value, replacement)

	written.tag(anyString, wireBytes)
	written.varint(uint64(len(value)))
	written = append(written, value...)

	return written
}

func intValue(value int64) []byte {
	written := message{}

	written.tag(anyInt, wireVarint)
	written.varint(uint64(value)) // #nosec G115 -- protobuf writes an int64 as its two's complement

	return written
}

// anyValue writes a resolved slog value as an AnyValue. A group is written
// by the caller, which knows where its attributes go.
func anyValue(value slog.Value) []byte {
	written := message{}

	switch value.Kind() {
	case slog.KindString:
		return stringValue(value.String())
	case slog.KindInt64:
		return intValue(value.Int64())
	case slog.KindUint64:
		// OTLP has no unsigned integer: one past the signed range goes as text.
		if value.Uint64() > math.MaxInt64 {
			return stringValue(strconv.FormatUint(value.Uint64(), 10))
		}

		return intValue(int64(value.Uint64())) // #nosec G115 -- in range, checked above
	case slog.KindFloat64:
		written.double(anyDouble, value.Float64())
	case slog.KindBool:
		written.tag(anyBool, wireVarint)

		if value.Bool() {
			written.varint(1)
		} else {
			written.varint(0)
		}
	case slog.KindDuration:
		// As slog's JSON handler writes it: nanoseconds.
		return intValue(int64(value.Duration()))
	case slog.KindTime:
		// As slog's JSON handler writes it.
		return stringValue(value.Time().Format(time.RFC3339Nano))
	case slog.KindGroup:
		written.nested(anyKVList, keyValues(value.Group()))
	case slog.KindAny, slog.KindLogValuer:
		return opaque(value.Any())
	}

	return written
}

// opaque writes a value slog has no kind for, as slog's own handlers do: an
// error as what it says, a text marshaler as its text, a Stringer as its
// string, anything else as fmt prints it.
//
// The value's own method may panic, and a nil pointer of a type that has one
// does: slog's handlers write "<nil>" for that pointer and the panic for any
// other, and so does this, in the program's own logging call.
func opaque(value any) (written []byte) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		if of := reflect.ValueOf(value); of.Kind() == reflect.Pointer && of.IsNil() {
			written = stringValue("<nil>")

			return
		}

		written = stringValue(fmt.Sprintf("!PANIC: %v", recovered))
	}()

	switch typed := value.(type) {
	case nil:
		// No value at all: an AnyValue with nothing set.
		return nil
	case []byte:
		bytes := message{}

		bytes.nested(anyBytes, typed)

		return bytes
	case error:
		return stringValue(typed.Error())
	case encoding.TextMarshaler:
		if text, err := typed.MarshalText(); err == nil {
			return stringValue(string(text))
		}
	case fmt.Stringer:
		return stringValue(typed.String())
	}

	return stringValue(fmt.Sprintf("%+v", value))
}

// keyValues writes attributes as the body of a KeyValueList, or of any
// message whose field 1 is repeated KeyValue. slog's rules hold: a value is
// resolved first, an attribute with no key and no value is left out, a group
// with no key is inlined, and a group with no attribute in it is left out.
func keyValues(attrs []slog.Attr) []byte {
	written := message{}

	for _, attr := range attrs {
		appendAttr(&written, listValues, attr)
	}

	return written
}

// appendAttr appends one attribute to a message whose given field is
// repeated KeyValue.
func appendAttr(into *message, field int, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()

	if attr.Equal(slog.Attr{}) {
		return
	}

	if attr.Value.Kind() != slog.KindGroup {
		into.nested(field, keyValue(attr.Key, anyValue(attr.Value)))

		return
	}

	group := attr.Value.Group()

	if attr.Key == "" {
		for _, member := range group {
			appendAttr(into, field, member)
		}

		return
	}

	if members := keyValues(group); len(members) > 0 {
		list := message{}

		list.nested(anyKVList, members)
		into.nested(field, keyValue(attr.Key, list))
	}
}
