package changefeed_test

import (
	"errors"
	"testing"

	"yodadb/changefeed"
	"yodadb/dberrors"
	"yodadb/types"
)

func TestRecordRoundTrip(t *testing.T) {
	tests := []changefeed.Change{
		{TxnID: 7, Sequence: 0, TableID: 3, Kind: changefeed.Insert, AfterKey: []byte("new"), After: image(2, "Ada")},
		{TxnID: 7, Sequence: 1, TableID: 3, Kind: changefeed.Update, BeforeKey: []byte("old"), AfterKey: []byte("new"), Before: image(2, "Ada"), After: image(2, "Grace")},
		{TxnID: 7, Sequence: 2, TableID: 3, Kind: changefeed.Delete, BeforeKey: []byte("old"), Before: image(2, "Grace")},
	}
	for _, want := range tests {
		encoded, err := changefeed.EncodeRecord(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := changefeed.DecodeRecord(want.TxnID, want.Sequence, encoded)
		if err != nil {
			t.Fatal(err)
		}
		assertChange(t, got, want)
	}
}

func TestRecordRejectsMalformedAndInvalidShapes(t *testing.T) {
	if _, err := changefeed.EncodeRecord(changefeed.Change{TxnID: 1, TableID: 1, Kind: changefeed.Insert}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("invalid insert error=%v", err)
	}
	valid, err := changefeed.EncodeRecord(changefeed.Change{TxnID: 1, TableID: 1, Kind: changefeed.Insert, AfterKey: []byte{1}, After: image(1, "Ada")})
	if err != nil {
		t.Fatal(err)
	}
	valid[0] ^= 0xff
	if _, err := changefeed.DecodeRecord(1, 0, valid); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("corrupt magic error=%v", err)
	}
}

func FuzzDecodeRecord(f *testing.F) {
	seed, err := changefeed.EncodeRecord(changefeed.Change{TxnID: 1, TableID: 1, Kind: changefeed.Insert, AfterKey: []byte{1}, After: image(1, "Ada")})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, encoded []byte) {
		_, _ = changefeed.DecodeRecord(1, 0, encoded)
	})
}

func image(version uint64, name string) *changefeed.Image {
	return &changefeed.Image{SchemaVersion: version, Row: types.Row{types.Int64(1), types.Text(name)}}
}

func assertChange(t *testing.T, got, want changefeed.Change) {
	t.Helper()
	if got.TxnID != want.TxnID || got.Sequence != want.Sequence || got.TableID != want.TableID || got.Kind != want.Kind || string(got.BeforeKey) != string(want.BeforeKey) || string(got.AfterKey) != string(want.AfterKey) {
		t.Fatalf("change metadata=%+v, want %+v", got, want)
	}
	assertImage := func(label string, got, want *changefeed.Image) {
		t.Helper()
		if got == nil || want == nil {
			if got != want {
				t.Fatalf("%s image=%v, want %v", label, got, want)
			}
			return
		}
		if got.SchemaVersion != want.SchemaVersion || len(got.Row) != len(want.Row) {
			t.Fatalf("%s image=%v, want %v", label, got, want)
		}
		for index := range got.Row {
			comparison, err := types.Compare(got.Row[index], want.Row[index])
			if err != nil || comparison != 0 {
				t.Fatalf("%s row=%v, want %v", label, got.Row, want.Row)
			}
		}
	}
	assertImage("before", got.Before, want.Before)
	assertImage("after", got.After, want.After)
}
