package types_test

import (
	"errors"
	"math"
	"testing"

	"yodadb/dberrors"
	"yodadb/types"
)

func TestEqualUsesSQLNullSemantics(t *testing.T) {
	result, err := types.Equal(types.Null(), types.Null())
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsNull() {
		t.Fatalf("NULL = NULL produced %v, want NULL", result)
	}
}

func TestCompareScalarKinds(t *testing.T) {
	tests := []struct {
		name        string
		left, right types.Value
		want        int
	}{
		{"null first", types.Null(), types.Int64(1), -1},
		{"false before true", types.Bool(false), types.Bool(true), -1},
		{"integers", types.Int64(4), types.Int64(4), 0},
		{"text binary order", types.Text("Ada"), types.Text("Grace"), -1},
		{"bytes lexicographic", types.Bytes([]byte{1, 2}), types.Bytes([]byte{1, 3}), -1},
		{"NaN after infinity", types.Float64(math.NaN()), types.Float64(math.Inf(1)), 1},
		{"NaNs equal", types.Float64(math.NaN()), types.Float64(math.NaN()), 0},
		{"signed zeros equal", types.Float64(math.Copysign(0, -1)), types.Float64(0), 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := types.Compare(test.left, test.right)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("Compare(%v, %v) = %d, want %d", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestCompareRejectsDifferentKinds(t *testing.T) {
	_, err := types.Compare(types.Int64(1), types.Text("1"))
	if !errors.Is(err, dberrors.ErrType) {
		t.Fatalf("Compare error = %v, want type error", err)
	}
}

func TestCoerceLosslessIntToFloat(t *testing.T) {
	value, err := types.CoerceLossless(types.Int64(42), types.Float64Kind)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := value.Float64Value(); !ok || got != 42 {
		t.Fatalf("coerced value = %v, want FLOAT64 42", value)
	}

	_, err = types.CoerceLossless(types.Int64(1<<53+1), types.Float64Kind)
	if !errors.Is(err, dberrors.ErrType) {
		t.Fatalf("lossy conversion error = %v, want type error", err)
	}
}

func TestBytesAreDefensivelyCopied(t *testing.T) {
	input := []byte{1, 2, 3}
	value := types.Bytes(input)
	input[0] = 9
	got, _ := value.BytesValue()
	if got[0] != 1 {
		t.Fatalf("stored bytes changed through input slice: %v", got)
	}
	got[1] = 9
	again, _ := value.BytesValue()
	if again[1] != 2 {
		t.Fatalf("stored bytes changed through output slice: %v", again)
	}
}

func TestFromGoRejectsUnsupportedAndOverflowingValues(t *testing.T) {
	for _, input := range []any{uint64(math.MaxUint64), struct{}{}} {
		if _, err := types.FromGo(input); !errors.Is(err, dberrors.ErrType) {
			t.Fatalf("FromGo(%T) error = %v, want type error", input, err)
		}
	}
}
