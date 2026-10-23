package types_test

import (
	"errors"
	"math"
	"testing"

	"yodadb/dberrors"
	"yodadb/types"
)

func TestTupleRoundTrip(t *testing.T) {
	row := types.Row{
		types.Null(),
		types.Bool(true),
		types.Int64(-42),
		types.Float64(math.NaN()),
		types.Text("Ada 世界"),
		types.Bytes([]byte{0, 1, 255}),
	}
	encoded, err := types.EncodeTuple(7, row, types.TupleLimits{})
	if err != nil {
		t.Fatal(err)
	}
	schemaVersion, decoded, err := types.DecodeTuple(encoded, types.TupleLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if schemaVersion != 7 || len(decoded) != len(row) {
		t.Fatalf("decoded schema/width = %d/%d, want 7/%d", schemaVersion, len(decoded), len(row))
	}
	for index := range row {
		comparison, err := types.Compare(row[index], decoded[index])
		if err != nil || comparison != 0 {
			t.Fatalf("column %d round trip = %v, want %v (error %v)", index, decoded[index], row[index], err)
		}
	}
}

func TestTupleLimits(t *testing.T) {
	limits := types.TupleLimits{MaxColumns: 1, MaxValueBytes: 3, MaxTupleBytes: 32}
	for _, row := range []types.Row{
		{types.Int64(1), types.Int64(2)},
		{types.Text("four")},
	} {
		_, err := types.EncodeTuple(1, row, limits)
		if !errors.Is(err, types.ErrValueTooLarge) || !errors.Is(err, dberrors.ErrResourceLimit) {
			t.Fatalf("EncodeTuple error = %v, want value-too-large resource error", err)
		}
	}
}

func TestDecodeTupleRejectsMalformedInputs(t *testing.T) {
	valid, err := types.EncodeTuple(1, types.Row{types.Int64(1)}, types.TupleLimits{})
	if err != nil {
		t.Fatal(err)
	}
	tests := [][]byte{
		nil,
		{2},
		{1, 0x80},
		valid[:len(valid)-1],
		append(append([]byte(nil), valid...), 0),
	}
	for index, input := range tests {
		_, _, err := types.DecodeTuple(input, types.TupleLimits{})
		if !errors.Is(err, dberrors.ErrCorruption) {
			t.Errorf("case %d error = %v, want corruption", index, err)
		}
	}
}

func TestDecodeTupleRejectsInvalidBoolAndUTF8(t *testing.T) {
	for _, row := range []types.Row{{types.Bool(true)}, {types.Text("a")}} {
		encoded, err := types.EncodeTuple(1, row, types.TupleLimits{})
		if err != nil {
			t.Fatal(err)
		}
		encoded[len(encoded)-1] = 0xff
		if _, _, err := types.DecodeTuple(encoded, types.TupleLimits{}); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("DecodeTuple error = %v, want corruption", err)
		}
	}
}

func TestDecodeTupleRejectsNoncanonicalHeaderBitsAndVarints(t *testing.T) {
	for _, encoded := range [][]byte{
		{1, 0x80, 0x00, 0},
		{1, 0, 1, 0x80, 0, 0},
	} {
		if _, _, err := types.DecodeTuple(encoded, types.TupleLimits{}); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("DecodeTuple(%x) error = %v, want corruption", encoded, err)
		}
	}
}

func TestDecodeTupleRejectsEveryTruncatedPrefix(t *testing.T) {
	valid, err := types.EncodeTuple(3, types.Row{types.Int64(1), types.Text("Ada")}, types.TupleLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for length := range len(valid) {
		if _, _, err := types.DecodeTuple(valid[:length], types.TupleLimits{}); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("prefix length %d error = %v, want corruption", length, err)
		}
	}
}

func FuzzDecodeTuple(f *testing.F) {
	valid, _ := types.EncodeTuple(1, types.Row{types.Int64(1), types.Text("Ada")}, types.TupleLimits{})
	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte{1, 0x80})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = types.DecodeTuple(data, types.TupleLimits{})
	})
}
