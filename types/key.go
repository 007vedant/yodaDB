package types

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf8"

	"yodadb/dberrors"
)

const (
	keyFormatVersion byte = 1
	keyEscape        byte = 0
	keyEscapedZero   byte = 0xff
)

// KeyLimits bounds key decoder allocations and encoded values.
type KeyLimits struct {
	MaxComponents int
	MaxValueBytes int
	MaxKeyBytes   int
}

// DefaultKeyLimits returns conservative version-1 resource limits.
func DefaultKeyLimits() KeyLimits {
	return KeyLimits{MaxComponents: 64, MaxValueBytes: 1 << 20, MaxKeyBytes: 4 << 20}
}

// KeyCorruptionError describes malformed persisted key bytes. Component is -1
// when the failure is in the key-level header.
type KeyCorruptionError struct {
	Offset    int
	Component int
	Invariant string
}

// Error implements error.
func (e *KeyCorruptionError) Error() string {
	if e.Component >= 0 {
		return fmt.Sprintf("key corruption at byte %d, component %d: %s", e.Offset, e.Component, e.Invariant)
	}
	return fmt.Sprintf("key corruption at byte %d: %s", e.Offset, e.Invariant)
}

// Unwrap categorizes malformed key bytes as corruption.
func (e *KeyCorruptionError) Unwrap() error { return dberrors.ErrCorruption }

// EncodeKey encodes a canonical composite key whose byte order matches the
// scalar order for corresponding components of the same declared type.
func EncodeKey(values Row, limits KeyLimits) ([]byte, error) {
	limits = normalizeKeyLimits(limits)
	if len(values) > limits.MaxComponents {
		return nil, keyLimitError("key has too many components")
	}
	encoded := make([]byte, 1, min(limits.MaxKeyBytes, 1+len(values)*2))
	encoded[0] = keyFormatVersion
	for component, value := range values {
		encoded = append(encoded, byte(value.Kind()))
		switch value.Kind() {
		case NullKind:
		case BoolKind:
			payload, _ := value.BoolValue()
			encoded = append(encoded, byteFromBool(payload))
		case Int64Kind:
			payload, _ := value.Int64Value()
			ordered := uint64(payload) ^ (uint64(1) << 63)
			encoded = binary.BigEndian.AppendUint64(encoded, ordered)
		case Float64Kind:
			payload, _ := value.Float64Value()
			encoded = binary.BigEndian.AppendUint64(encoded, orderedFloatBits(payload))
		case TextKind:
			payload, _ := value.TextValue()
			if !utf8.ValidString(payload) {
				return nil, dberrors.New(dberrors.CodeType, "key.encode", fmt.Sprintf("component %d TEXT is not valid UTF-8", component))
			}
			if len(payload) > limits.MaxValueBytes {
				return nil, keyLimitError(fmt.Sprintf("component %d TEXT exceeds value limit", component))
			}
			encoded = appendEscapedKeyBytes(encoded, []byte(payload))
		case BytesKind:
			payload, _ := value.BytesValue()
			if len(payload) > limits.MaxValueBytes {
				return nil, keyLimitError(fmt.Sprintf("component %d BYTES exceeds value limit", component))
			}
			encoded = appendEscapedKeyBytes(encoded, payload)
		default:
			return nil, dberrors.New(dberrors.CodeType, "key.encode", fmt.Sprintf("component %d has invalid kind %s", component, value.Kind()))
		}
		if len(encoded) > limits.MaxKeyBytes {
			return nil, keyLimitError("encoded key exceeds key limit")
		}
	}
	return encoded, nil
}

// DecodeKey validates and decodes a canonical version-1 composite key.
func DecodeKey(encoded []byte, limits KeyLimits) (Row, error) {
	limits = normalizeKeyLimits(limits)
	if len(encoded) > limits.MaxKeyBytes {
		return nil, keyLimitError("encoded key exceeds key limit")
	}
	if len(encoded) == 0 {
		return nil, keyCorrupt(0, -1, "missing key format version")
	}
	if encoded[0] != keyFormatVersion {
		return nil, keyCorrupt(0, -1, fmt.Sprintf("unsupported key format version %d", encoded[0]))
	}

	row := make(Row, 0)
	offset := 1
	for offset < len(encoded) {
		component := len(row)
		if component >= limits.MaxComponents {
			return nil, keyLimitError("key has too many components")
		}
		tagOffset := offset
		kind := Kind(encoded[offset])
		offset++
		var value Value
		switch kind {
		case NullKind:
			value = Null()
		case BoolKind:
			if offset >= len(encoded) {
				return nil, keyCorrupt(offset, component, "truncated BOOL payload")
			}
			if encoded[offset] > 1 {
				return nil, keyCorrupt(offset, component, "BOOL payload must be 0 or 1")
			}
			value = Bool(encoded[offset] == 1)
			offset++
		case Int64Kind:
			payload, next, err := fixedKeyPayload(encoded, offset, component, "INT64")
			if err != nil {
				return nil, err
			}
			ordered := binary.BigEndian.Uint64(payload)
			value = Int64(int64(ordered ^ (uint64(1) << 63)))
			offset = next
		case Float64Kind:
			payload, next, err := fixedKeyPayload(encoded, offset, component, "FLOAT64")
			if err != nil {
				return nil, err
			}
			ordered := binary.BigEndian.Uint64(payload)
			decoded, canonical := decodeOrderedFloat(ordered)
			if !canonical {
				return nil, keyCorrupt(offset, component, "FLOAT64 payload is not canonical")
			}
			value = Float64(decoded)
			offset = next
		case TextKind, BytesKind:
			payload, next, err := decodeEscapedKeyBytes(encoded, offset, component, limits.MaxValueBytes)
			if err != nil {
				return nil, err
			}
			if kind == TextKind {
				if !utf8.Valid(payload) {
					return nil, keyCorrupt(offset, component, "TEXT payload is not valid UTF-8")
				}
				value = Text(string(payload))
			} else {
				value = Bytes(payload)
			}
			offset = next
		default:
			return nil, keyCorrupt(tagOffset, component, fmt.Sprintf("unknown type tag %d", kind))
		}
		row = append(row, value)
	}
	return row, nil
}

func appendEscapedKeyBytes(encoded, payload []byte) []byte {
	for _, value := range payload {
		if value == keyEscape {
			encoded = append(encoded, keyEscape, keyEscapedZero)
		} else {
			encoded = append(encoded, value)
		}
	}
	return append(encoded, keyEscape, keyEscape)
}

func decodeEscapedKeyBytes(encoded []byte, offset, component, maximum int) ([]byte, int, error) {
	decoded := make([]byte, 0)
	for offset < len(encoded) {
		value := encoded[offset]
		offset++
		if value != keyEscape {
			decoded = append(decoded, value)
		} else {
			if offset >= len(encoded) {
				return nil, 0, keyCorrupt(offset-1, component, "truncated variable-width escape")
			}
			escape := encoded[offset]
			offset++
			switch escape {
			case keyEscape:
				return decoded, offset, nil
			case keyEscapedZero:
				decoded = append(decoded, 0)
			default:
				return nil, 0, keyCorrupt(offset-1, component, fmt.Sprintf("invalid variable-width escape %02x", escape))
			}
		}
		if len(decoded) > maximum {
			return nil, 0, keyLimitError(fmt.Sprintf("component %d exceeds value limit", component))
		}
	}
	return nil, 0, keyCorrupt(offset, component, "missing variable-width terminator")
}

func fixedKeyPayload(encoded []byte, offset, component int, description string) ([]byte, int, error) {
	const width = 8
	if offset > len(encoded)-width {
		return nil, 0, keyCorrupt(offset, component, "truncated "+description+" payload")
	}
	return encoded[offset : offset+width], offset + width, nil
}

func orderedFloatBits(value float64) uint64 {
	if math.IsNaN(value) {
		return math.MaxUint64
	}
	if value == 0 {
		value = 0
	}
	bits := math.Float64bits(value)
	if bits&(uint64(1)<<63) != 0 {
		return ^bits
	}
	return bits ^ (uint64(1) << 63)
}

func decodeOrderedFloat(ordered uint64) (float64, bool) {
	if ordered == math.MaxUint64 {
		return math.NaN(), true
	}
	bits := ordered ^ (uint64(1) << 63)
	if ordered&(uint64(1)<<63) == 0 {
		bits = ^ordered
	}
	value := math.Float64frombits(bits)
	if math.IsNaN(value) || bits == (uint64(1)<<63) {
		return 0, false
	}
	return value, true
}

func byteFromBool(value bool) byte {
	if value {
		return 1
	}
	return 0
}

func normalizeKeyLimits(limits KeyLimits) KeyLimits {
	defaults := DefaultKeyLimits()
	if limits.MaxComponents <= 0 {
		limits.MaxComponents = defaults.MaxComponents
	}
	if limits.MaxValueBytes <= 0 {
		limits.MaxValueBytes = defaults.MaxValueBytes
	}
	if limits.MaxKeyBytes <= 0 {
		limits.MaxKeyBytes = defaults.MaxKeyBytes
	}
	return limits
}

func keyLimitError(message string) error {
	return dberrors.Wrap(ErrValueTooLarge, dberrors.CodeResourceLimit, "key.limit", message)
}

func keyCorrupt(offset, component int, invariant string) error {
	return &KeyCorruptionError{Offset: offset, Component: component, Invariant: invariant}
}
