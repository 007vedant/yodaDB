package page_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

func TestInternalBuilderRoundTrip(t *testing.T) {
	builder := page.NewInternalBuilder(2)
	if err := builder.Insert(0, []byte("b"), 3); err != nil {
		t.Fatal(err)
	}
	if err := builder.Insert(1, []byte("d"), 4); err != nil {
		t.Fatal(err)
	}
	payload, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	internal, err := page.DecodeInternal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if internal.FirstChild != 2 || len(internal.Entries) != 2 {
		t.Fatalf("decoded internal = %#v", internal)
	}
	if string(internal.Entries[0].Key) != "b" || internal.Entries[0].RightChild != 3 ||
		string(internal.Entries[1].Key) != "d" || internal.Entries[1].RightChild != 4 {
		t.Fatalf("decoded entries = %#v", internal.Entries)
	}

	rebuilt, err := page.NewInternalBuilderFromDecoded(internal).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(rebuilt) != string(payload) {
		t.Fatal("decode/rebuild did not preserve canonical internal bytes")
	}
}

func TestInternalBuilderDeleteAndBounds(t *testing.T) {
	builder := page.NewInternalBuilder(2)
	if err := builder.Insert(0, []byte("b"), 3); err != nil {
		t.Fatal(err)
	}
	if err := builder.Insert(1, []byte("d"), 4); err != nil {
		t.Fatal(err)
	}
	if err := builder.Delete(0); err != nil {
		t.Fatal(err)
	}
	entry, err := builder.Entry(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(entry.Key) != "d" || entry.RightChild != 4 {
		t.Fatalf("remaining entry = %#v", entry)
	}
	for _, err := range []error{
		builder.Insert(2, []byte("x"), 5),
		builder.Insert(1, nil, 5),
		builder.Insert(1, []byte("x"), 1),
		builder.Delete(1),
	} {
		if !errors.Is(err, dberrors.ErrConstraint) {
			t.Errorf("operation error = %v, want constraint", err)
		}
	}
	if err := builder.Insert(1, make([]byte, page.MaxPayloadSize), 5); !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("oversized insert error = %v, want resource limit", err)
	}
}

func TestInternalBuilderRejectsInvalidTree(t *testing.T) {
	if _, err := page.NewInternalBuilder(1).Encode(); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("reserved first child error = %v, want constraint", err)
	}
	empty := page.NewInternalBuilder(2)
	if _, err := empty.Encode(); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("empty internal error = %v, want constraint", err)
	}
	unordered := page.NewInternalBuilder(2)
	_ = unordered.Insert(0, []byte("z"), 3)
	_ = unordered.Insert(1, []byte("a"), 4)
	if _, err := unordered.Encode(); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("unordered separator error = %v, want constraint", err)
	}
}

func TestDecodeInternalRejectsStructuralCorruption(t *testing.T) {
	valid := validInternalPayload(t)
	reservedChild := append([]byte(nil), valid...)
	binary.LittleEndian.PutUint64(reservedChild[8:16], 1)
	badFlags := append([]byte(nil), valid...)
	badFlags[20] = 1
	badOrder := append([]byte(nil), valid...)
	secondKeyOffset := int(binary.LittleEndian.Uint16(badOrder[32:34]))
	badOrder[secondKeyOffset] = 'a'
	for index, payload := range [][]byte{
		valid[:len(valid)-1],
		mutateInternal(valid, 0, 2),
		mutateInternal(valid, 1, 1),
		mutateInternal(valid, 4, 1),
		reservedChild,
		badFlags,
		badOrder,
	} {
		if _, err := page.DecodeInternal(payload); !errors.Is(err, dberrors.ErrCorruption) {
			t.Errorf("case %d error = %v, want corruption", index, err)
		}
	}
}

func TestDecodeInternalRejectsEveryTruncatedPrefix(t *testing.T) {
	valid := validInternalPayload(t)
	for length := range len(valid) {
		if _, err := page.DecodeInternal(valid[:length]); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("prefix length %d error = %v, want corruption", length, err)
		}
	}
}

func FuzzDecodeInternal(f *testing.F) {
	builder := page.NewInternalBuilder(2)
	_ = builder.Insert(0, []byte("key"), 3)
	valid, _ := builder.Encode()
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, page.MaxPayloadSize))
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = page.DecodeInternal(payload)
	})
}

func validInternalPayload(t *testing.T) []byte {
	t.Helper()
	builder := page.NewInternalBuilder(2)
	if err := builder.Insert(0, []byte("a"), 3); err != nil {
		t.Fatal(err)
	}
	if err := builder.Insert(1, []byte("b"), 4); err != nil {
		t.Fatal(err)
	}
	payload, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mutateInternal(payload []byte, offset int, xor byte) []byte {
	mutated := append([]byte(nil), payload...)
	mutated[offset] ^= xor
	return mutated
}
