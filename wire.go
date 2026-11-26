package telemetry

import (
	"encoding/binary"
	"math"
	"strings"
)

// The protobuf wire types OTLP's messages use
// (https://protobuf.dev/programming-guides/encoding/).
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
)

// message is a protobuf message being written. OTLP's messages are few and
// fixed, so each is written field by field, with no reflection and no
// generated code.
type message []byte

func (m *message) varint(value uint64) {
	*m = binary.AppendUvarint(*m, value)
}

func (m *message) tag(field, wire int) {
	m.varint(uint64(field)<<3 | uint64(wire)) // #nosec G115 -- a field number, a small constant
}

// uint writes a varint field, and nothing for zero, which is the field's
// default.
func (m *message) uint(field int, value uint64) {
	if value == 0 {
		return
	}

	m.tag(field, wireVarint)
	m.varint(value)
}

// fixed writes a fixed64 field, zero included: where a zero is the default
// and can be left out, the caller leaves it out.
func (m *message) fixed(field int, value uint64) {
	m.tag(field, wireFixed64)
	*m = binary.LittleEndian.AppendUint64(*m, value)
}

func (m *message) double(field int, value float64) {
	m.fixed(field, math.Float64bits(value))
}

// replacement stands for each run of bytes that is no UTF-8 in a string: a
// protobuf string is UTF-8, and a decoder refuses the whole message of one
// that is not.
const replacement = "\uFFFD"

// text writes a string field, and nothing for the empty string, which is
// the field's default.
func (m *message) text(field int, value string) {
	if value == "" {
		return
	}

	value = strings.ToValidUTF8(value, replacement)

	m.tag(field, wireBytes)
	m.varint(uint64(len(value)))
	*m = append(*m, value...)
}

// nested writes a field that holds a message already written, empty or not.
func (m *message) nested(field int, body []byte) {
	m.tag(field, wireBytes)
	m.varint(uint64(len(body)))
	*m = append(*m, body...)
}
