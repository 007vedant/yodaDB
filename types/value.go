package types

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	"yodadb/dberrors"
)

// Kind identifies a scalar value type.
type Kind uint8

const (
	// NullKind identifies the absence of a value.
	NullKind Kind = iota
	// BoolKind identifies a Boolean value.
	BoolKind
	// Int64Kind identifies a signed 64-bit integer.
	Int64Kind
	// Float64Kind identifies an IEEE 754 double-precision number.
	Float64Kind
	// TextKind identifies a valid UTF-8 string.
	TextKind
	// BytesKind identifies an arbitrary byte sequence.
	BytesKind
)

// String returns the SQL spelling of a kind.
func (k Kind) String() string {
	switch k {
	case NullKind:
		return "NULL"
	case BoolKind:
		return "BOOL"
	case Int64Kind:
		return "INT64"
	case Float64Kind:
		return "FLOAT64"
	case TextKind:
		return "TEXT"
	case BytesKind:
		return "BYTES"
	default:
		return fmt.Sprintf("Kind(%d)", k)
	}
}

// Value is a strictly typed scalar database value.
type Value struct {
	kind  Kind
	bool  bool
	int64 int64
	float float64
	text  string
	bytes []byte
}

// Row is an ordered collection of values.
type Row []Value

// Null constructs a NULL value.
func Null() Value { return Value{kind: NullKind} }

// Bool constructs a BOOL value.
func Bool(value bool) Value { return Value{kind: BoolKind, bool: value} }

// Int64 constructs an INT64 value.
func Int64(value int64) Value { return Value{kind: Int64Kind, int64: value} }

// Float64 constructs a FLOAT64 value.
func Float64(value float64) Value { return Value{kind: Float64Kind, float: value} }

// Text constructs a TEXT value.
func Text(value string) Value { return Value{kind: TextKind, text: value} }

// Bytes constructs a BYTES value and takes a defensive copy.
func Bytes(value []byte) Value {
	return Value{kind: BytesKind, bytes: bytes.Clone(value)}
}

// Kind returns the value's scalar kind.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether the value is NULL.
func (v Value) IsNull() bool { return v.kind == NullKind }

// BoolValue returns the BOOL payload.
func (v Value) BoolValue() (bool, bool) { return v.bool, v.kind == BoolKind }

// Int64Value returns the INT64 payload.
func (v Value) Int64Value() (int64, bool) { return v.int64, v.kind == Int64Kind }

// Float64Value returns the FLOAT64 payload.
func (v Value) Float64Value() (float64, bool) { return v.float, v.kind == Float64Kind }

// TextValue returns the TEXT payload.
func (v Value) TextValue() (string, bool) { return v.text, v.kind == TextKind }

// BytesValue returns a copy of the BYTES payload.
func (v Value) BytesValue() ([]byte, bool) {
	if v.kind != BytesKind {
		return nil, false
	}
	return bytes.Clone(v.bytes), true
}

// GoValue returns the closest ordinary Go representation of v.
func (v Value) GoValue() any {
	switch v.kind {
	case NullKind:
		return nil
	case BoolKind:
		return v.bool
	case Int64Kind:
		return v.int64
	case Float64Kind:
		return v.float
	case TextKind:
		return v.text
	case BytesKind:
		return bytes.Clone(v.bytes)
	default:
		return nil
	}
}

// String returns a diagnostic representation of v.
func (v Value) String() string {
	switch v.kind {
	case NullKind:
		return "NULL"
	case BoolKind:
		return strconv.FormatBool(v.bool)
	case Int64Kind:
		return strconv.FormatInt(v.int64, 10)
	case Float64Kind:
		return strconv.FormatFloat(v.float, 'g', -1, 64)
	case TextKind:
		return v.text
	case BytesKind:
		return fmt.Sprintf("%x", v.bytes)
	default:
		return "<invalid>"
	}
}

// Compare returns -1, 0, or 1 using the total ordering for a scalar kind. NULL
// sorts before non-NULL values. Different non-NULL kinds are not comparable.
func Compare(left, right Value) (int, error) {
	if left.IsNull() || right.IsNull() {
		switch {
		case left.IsNull() && right.IsNull():
			return 0, nil
		case left.IsNull():
			return -1, nil
		default:
			return 1, nil
		}
	}
	if left.kind != right.kind {
		return 0, dberrors.New(dberrors.CodeType, "types.compare", fmt.Sprintf("cannot compare %s and %s", left.kind, right.kind))
	}

	switch left.kind {
	case BoolKind:
		if left.bool == right.bool {
			return 0, nil
		}
		if !left.bool {
			return -1, nil
		}
		return 1, nil
	case Int64Kind:
		return compareInt64(left.int64, right.int64), nil
	case Float64Kind:
		return compareFloat(left.float, right.float), nil
	case TextKind:
		return bytes.Compare([]byte(left.text), []byte(right.text)), nil
	case BytesKind:
		return bytes.Compare(left.bytes, right.bytes), nil
	default:
		return 0, dberrors.New(dberrors.CodeType, "types.compare", "invalid value kind")
	}
}

// Equal evaluates SQL equality. If either operand is NULL, the result is NULL.
func Equal(left, right Value) (Value, error) {
	if left.IsNull() || right.IsNull() {
		return Null(), nil
	}
	comparison, err := Compare(left, right)
	if err != nil {
		return Value{}, err
	}
	return Bool(comparison == 0), nil
}

// CoerceLossless converts value to target only when the conversion cannot lose
// information. NULL can inhabit every scalar column.
func CoerceLossless(value Value, target Kind) (Value, error) {
	if value.IsNull() || value.kind == target {
		return value, nil
	}
	if value.kind == Int64Kind && target == Float64Kind {
		converted := float64(value.int64)
		if int64(converted) == value.int64 {
			return Float64(converted), nil
		}
	}
	return Value{}, dberrors.New(dberrors.CodeType, "types.coerce", fmt.Sprintf("cannot losslessly convert %s to %s", value.kind, target))
}

// FromGo converts supported public-API argument values to strict database
// values. Platform-sized integers are accepted only when representable as int64.
func FromGo(value any) (Value, error) {
	switch value := value.(type) {
	case nil:
		return Null(), nil
	case bool:
		return Bool(value), nil
	case int:
		return Int64(int64(value)), nil
	case int8:
		return Int64(int64(value)), nil
	case int16:
		return Int64(int64(value)), nil
	case int32:
		return Int64(int64(value)), nil
	case int64:
		return Int64(value), nil
	case uint:
		if uint64(value) <= math.MaxInt64 {
			return Int64(int64(value)), nil
		}
	case uint8:
		return Int64(int64(value)), nil
	case uint16:
		return Int64(int64(value)), nil
	case uint32:
		return Int64(int64(value)), nil
	case uint64:
		if value <= math.MaxInt64 {
			return Int64(int64(value)), nil
		}
	case float32:
		return Float64(float64(value)), nil
	case float64:
		return Float64(value), nil
	case string:
		return Text(value), nil
	case []byte:
		return Bytes(value), nil
	case Value:
		return value, nil
	}
	return Value{}, dberrors.New(dberrors.CodeType, "types.from_go", fmt.Sprintf("unsupported Go value %T", value))
}

func compareInt64(left, right int64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

func compareFloat(left, right float64) int {
	leftNaN, rightNaN := math.IsNaN(left), math.IsNaN(right)
	switch {
	case leftNaN && rightNaN:
		return 0
	case leftNaN:
		return 1
	case rightNaN:
		return -1
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
