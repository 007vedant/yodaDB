package page_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

func TestLeafBuilderInsertDeleteRoundTrip(t *testing.T) {
	builder := page.NewLeafBuilder(9)
	if err := builder.Insert(0, []byte("b"), []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := builder.Insert(0, []byte("a"), []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := builder.Insert(2, []byte("c"), []byte("three")); err != nil {
		t.Fatal(err)
	}
	if err := builder.Delete(1); err != nil {
		t.Fatal(err)
	}
	payload, err := builder.Encode()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := page.DecodeLeaf(payload)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.RightSibling != 9 || len(leaf.Entries) != 2 {
		t.Fatalf("decoded leaf = %#v", leaf)
	}
	if string(leaf.Entries[0].Key) != "a" || string(leaf.Entries[1].Key) != "c" {
		t.Fatalf("logical entry order = %q, %q; want a, c", leaf.Entries[0].Key, leaf.Entries[1].Key)
	}

	rebuilt := page.NewLeafBuilderFromDecoded(leaf)
	rebuiltPayload, err := rebuilt.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(rebuiltPayload) != string(payload) {
		t.Fatal("decode/rebuild did not preserve canonical leaf bytes")
	}
}

func TestLeafPayloadWrapsInCommonPage(t *testing.T) {
	builder := page.NewLeafBuilder(0)
	if err := builder.Insert(0, []byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	payload, _ := builder.Encode()
	encoded, err := page.Encode(page.Header{Type: page.TypeBTreeLeaf, PageID: 2}, payload)
	if err != nil {
		t.Fatal(err)
	}
	decodedPage, err := page.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := page.DecodeLeaf(decodedPage.Payload)
	if err != nil || string(leaf.Entries[0].Value) != "value" {
		t.Fatalf("decoded leaf = %#v, error %v", leaf, err)
	}
}

func TestLeafBuilderEnforcesCapacityAndBounds(t *testing.T) {
	builder := page.NewLeafBuilder(0)
	if err := builder.Insert(1, nil, nil); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("out-of-range insert error = %v, want constraint", err)
	}
	if err := builder.Insert(0, []byte("key"), make([]byte, page.MaxPayloadSize)); !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("oversized insert error = %v, want resource limit", err)
	}
	if err := builder.Delete(0); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("out-of-range delete error = %v, want constraint", err)
	}
}

func TestDecodeLeafRejectsStructuralCorruption(t *testing.T) {
	builder := page.NewLeafBuilder(0)
	if err := builder.Insert(0, []byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	valid, _ := builder.Encode()
	tests := [][]byte{
		valid[:len(valid)-1],
		mutateLeaf(valid, 0, 2),
		mutateLeaf(valid, 1, 1),
		mutateLeaf(valid, 4, 1),
		mutateLeaf(valid, 20, 1),
	}
	for index, payload := range tests {
		if _, err := page.DecodeLeaf(payload); !errors.Is(err, dberrors.ErrCorruption) {
			t.Errorf("case %d error = %v, want corruption", index, err)
		}
	}
}

func TestDecodeLeafRejectsNoncanonicalKeyLength(t *testing.T) {
	builder := page.NewLeafBuilder(0)
	if err := builder.Insert(0, []byte("k"), []byte("v")); err != nil {
		t.Fatal(err)
	}
	payload, _ := builder.Encode()
	recordOffset := int(binary.LittleEndian.Uint16(payload[16:18]))
	payload[recordOffset] = 0x81
	if _, err := page.DecodeLeaf(payload); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("DecodeLeaf error = %v, want corruption", err)
	}
}

func TestDecodeLeafRejectsEveryTruncatedPrefix(t *testing.T) {
	builder := page.NewLeafBuilder(0)
	if err := builder.Insert(0, []byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	valid, _ := builder.Encode()
	for length := range len(valid) {
		if _, err := page.DecodeLeaf(valid[:length]); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("prefix length %d error = %v, want corruption", length, err)
		}
	}
}

func FuzzDecodeLeaf(f *testing.F) {
	builder := page.NewLeafBuilder(0)
	_ = builder.Insert(0, []byte("key"), []byte("value"))
	valid, _ := builder.Encode()
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, page.MaxPayloadSize))
	f.Fuzz(func(t *testing.T, payload []byte) {
		_, _ = page.DecodeLeaf(payload)
	})
}

func mutateLeaf(payload []byte, offset int, xor byte) []byte {
	mutated := append([]byte(nil), payload...)
	mutated[offset] ^= xor
	return mutated
}
