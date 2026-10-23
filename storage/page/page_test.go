package page_test

import (
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/page"
)

func TestPageRoundTrip(t *testing.T) {
	header := page.Header{Type: page.TypeCatalog, PageID: 7, Generation: 11}
	payload := []byte("catalog payload")
	encoded, err := page.Encode(header, payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := page.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Header.Type != header.Type || decoded.Header.PageID != header.PageID || decoded.Header.Generation != header.Generation {
		t.Fatalf("decoded header = %#v, want %#v", decoded.Header, header)
	}
	if string(decoded.Payload) != string(payload) {
		t.Fatalf("decoded payload = %q, want %q", decoded.Payload, payload)
	}
	encoded[page.HeaderSize] ^= 0xff
	if string(decoded.Payload) != string(payload) {
		t.Fatal("decoded payload aliases encoded page")
	}
}

func TestPageGoldenPrefix(t *testing.T) {
	encoded, err := page.Encode(page.Header{Type: page.TypeCatalog, PageID: 2, Generation: 7}, []byte("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	got := hex.EncodeToString(encoded[:page.HeaderSize+len("fixture")])
	fixture, err := os.ReadFile("testdata/page_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(fixture))
	if got != want {
		t.Fatalf("golden prefix = %q", got)
	}
}

func TestEverySingleByteMutationIsRejected(t *testing.T) {
	encoded, err := page.Encode(page.Header{Type: page.TypeCatalog, PageID: 2, Generation: 7}, []byte("mutation fixture"))
	if err != nil {
		t.Fatal(err)
	}
	for offset := range encoded {
		mutated := append([]byte(nil), encoded...)
		mutated[offset] ^= 1
		if _, err := page.Decode(mutated); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("mutation at byte %d error = %v, want corruption", offset, err)
		}
	}
}

func TestPageRejectsInvalidEncodeAndDecodeInputs(t *testing.T) {
	if _, err := page.Encode(page.Header{}, nil); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("invalid type error = %v, want constraint", err)
	}
	if _, err := page.Encode(page.Header{Type: page.TypeCatalog}, make([]byte, page.MaxPayloadSize+1)); !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("large payload error = %v, want resource limit", err)
	}
	if _, err := page.Decode(make([]byte, page.Size-1)); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("short page error = %v, want corruption", err)
	}
}

func TestPageRejectsEveryTruncatedPrefix(t *testing.T) {
	valid, err := page.Encode(page.Header{Type: page.TypeCatalog, PageID: 2}, []byte("prefix fixture"))
	if err != nil {
		t.Fatal(err)
	}
	for length := range len(valid) {
		if _, err := page.Decode(valid[:length]); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("prefix length %d error = %v, want corruption", length, err)
		}
	}
}

func FuzzDecodePage(f *testing.F) {
	valid, _ := page.Encode(page.Header{Type: page.TypeCatalog, PageID: 2}, []byte("seed"))
	f.Add(valid)
	f.Add([]byte{})
	f.Add(make([]byte, page.Size))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = page.Decode(data)
	})
}
