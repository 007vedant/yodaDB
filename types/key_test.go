package types_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"math"
	"testing"

	"yodadb/dberrors"
	"yodadb/types"
)

func TestKeyGoldenEncoding(t *testing.T) {
	encoded, err := types.EncodeKey(types.Row{
		types.Null(),
		types.Bool(true),
		types.Int64(-1),
		types.Float64(0),
		types.Text("a\x00"),
		types.Bytes([]byte{0xff}),
	}, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	const want = "01000101027fffffffffffffff038000000000000000046100ff000005ff0000"
	if got := hex.EncodeToString(encoded); got != want {
		t.Fatalf("encoded key = %s, want %s", got, want)
	}
}

func TestKeyRoundTrip(t *testing.T) {
	want := types.Row{
		types.Null(),
		types.Bool(true),
		types.Int64(-42),
		types.Float64(math.NaN()),
		types.Text("Ada\x00世界"),
		types.Bytes([]byte{0, 1, 255}),
	}
	encoded, err := types.EncodeKey(want, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := types.DecodeKey(encoded, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("decoded width = %d, want %d", len(got), len(want))
	}
	for index := range want {
		comparison, err := types.Compare(got[index], want[index])
		if err != nil || comparison != 0 {
			t.Fatalf("component %d = %v, want %v (error %v)", index, got[index], want[index], err)
		}
	}
}

func TestKeyEncodingPreservesScalarOrder(t *testing.T) {
	tests := [][]types.Value{
		{types.Null(), types.Bool(false), types.Bool(true)},
		{types.Null(), types.Int64(math.MinInt64), types.Int64(-1), types.Int64(0), types.Int64(1), types.Int64(math.MaxInt64)},
		{types.Null(), types.Float64(math.Inf(-1)), types.Float64(-1), types.Float64(math.Copysign(0, -1)), types.Float64(1), types.Float64(math.Inf(1)), types.Float64(math.NaN())},
		{types.Null(), types.Text(""), types.Text("a"), types.Text("a\x00"), types.Text("aa"), types.Text("世界")},
		{types.Null(), types.Bytes(nil), types.Bytes([]byte{0}), types.Bytes([]byte{0, 0}), types.Bytes([]byte{1}), types.Bytes([]byte{255})},
	}
	for _, values := range tests {
		for index := 0; index+1 < len(values); index++ {
			left, err := types.EncodeKey(types.Row{values[index]}, types.KeyLimits{})
			if err != nil {
				t.Fatal(err)
			}
			right, err := types.EncodeKey(types.Row{values[index+1]}, types.KeyLimits{})
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Compare(left, right) >= 0 {
				t.Fatalf("encoded %v (%x) does not sort before %v (%x)", values[index], left, values[index+1], right)
			}
		}
	}
}

func TestCompositeKeyOrderingAndPrefix(t *testing.T) {
	prefix, err := types.EncodeKey(types.Row{types.Text("Ada")}, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := types.EncodeKey(types.Row{types.Text("Ada"), types.Int64(1)}, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := types.EncodeKey(types.Row{types.Text("Ada"), types.Int64(2)}, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(first, prefix) || !bytes.HasPrefix(second, prefix) {
		t.Fatalf("composite encodings %x and %x do not share prefix %x", first, second, prefix)
	}
	if bytes.Compare(first, second) >= 0 {
		t.Fatalf("composite key 1 (%x) does not sort before key 2 (%x)", first, second)
	}
}

func TestKeyCanonicalizesNaNAndZero(t *testing.T) {
	encode := func(value float64) []byte {
		t.Helper()
		encoded, err := types.EncodeKey(types.Row{types.Float64(value)}, types.KeyLimits{})
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	}
	if !bytes.Equal(encode(0), encode(math.Copysign(0, -1))) {
		t.Fatal("positive and negative zero have different key encodings")
	}
	if !bytes.Equal(encode(math.NaN()), encode(math.Float64frombits(0x7ff0000000000001))) {
		t.Fatal("NaN payloads have different key encodings")
	}
	if bytes.Compare(encode(math.Inf(1)), encode(math.NaN())) >= 0 {
		t.Fatal("positive infinity does not sort before NaN")
	}
}

func TestDecodeKeyRejectsMalformedInputs(t *testing.T) {
	validInt, err := types.EncodeKey(types.Row{types.Int64(1)}, types.KeyLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, encoded := range [][]byte{
		nil,
		{2},
		{1, 99},
		{1, byte(types.BoolKind)},
		{1, byte(types.BoolKind), 2},
		validInt[:len(validInt)-1],
		{1, byte(types.TextKind), 'a'},
		{1, byte(types.BytesKind), 0, 1},
		{1, byte(types.TextKind), 0xff, 0, 0},
		{1, byte(types.Float64Kind), 0xff, 0xf8, 0, 0, 0, 0, 0, 1},
		{1, byte(types.Float64Kind), 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	} {
		if _, err := types.DecodeKey(encoded, types.KeyLimits{}); !errors.Is(err, dberrors.ErrCorruption) {
			t.Errorf("DecodeKey(%x) error = %v, want corruption", encoded, err)
		}
	}
}

func TestKeyLimits(t *testing.T) {
	limits := types.KeyLimits{MaxComponents: 1, MaxValueBytes: 2, MaxKeyBytes: 8}
	for _, row := range []types.Row{
		{types.Int64(1), types.Int64(2)},
		{types.Text("abc")},
		{types.Int64(1)},
	} {
		if _, err := types.EncodeKey(row, limits); !errors.Is(err, types.ErrValueTooLarge) || !errors.Is(err, dberrors.ErrResourceLimit) {
			t.Errorf("EncodeKey(%v) error = %v, want value-too-large resource error", row, err)
		}
	}
}

func FuzzDecodeKey(f *testing.F) {
	valid, _ := types.EncodeKey(types.Row{types.Int64(1), types.Text("Ada")}, types.KeyLimits{})
	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte{1, byte(types.BytesKind), 0})
	f.Fuzz(func(t *testing.T, encoded []byte) {
		_, _ = types.DecodeKey(encoded, types.KeyLimits{})
	})
}
