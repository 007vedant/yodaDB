package manifest_test

import (
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

func testFileID() manifest.FileID {
	return manifest.FileID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
}

func TestMetapageRoundTrip(t *testing.T) {
	meta := manifest.Meta{FileID: testFileID(), PageSize: page.Size, LatestTxnID: 7, LatestManifestPageID: 12, OldestRetainedTxnID: 3}
	encoded, err := manifest.EncodeMetapage(1, 9, meta)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := manifest.DecodeMetapage(encoded, 1)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Slot != 1 || candidate.Generation != 9 || candidate.Meta != meta {
		t.Fatalf("decoded candidate = %#v, want slot 1 generation 9 meta %#v", candidate, meta)
	}
}

func TestMetapageRejectsInvalidSemantics(t *testing.T) {
	if _, err := manifest.EncodeMetapage(2, 0, manifest.Meta{FileID: testFileID(), PageSize: page.Size}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("invalid slot error = %v, want constraint", err)
	}
	encoded, err := manifest.EncodeMetapage(0, 1, manifest.Meta{FileID: testFileID(), PageSize: page.Size})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.DecodeMetapage(encoded, 1); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("wrong physical slot error = %v, want corruption", err)
	}
	nonEmpty, err := manifest.EncodeMetapage(0, 2, manifest.Meta{FileID: testFileID(), PageSize: page.Size, LatestTxnID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.DecodeMetapage(nonEmpty, 0); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("missing manifest error=%v, want corruption", err)
	}
}

func TestDecodeMetapageRejectsEveryTruncatedPrefix(t *testing.T) {
	valid, err := manifest.EncodeMetapage(0, 1, manifest.Meta{FileID: testFileID(), PageSize: page.Size})
	if err != nil {
		t.Fatal(err)
	}
	for length := range len(valid) {
		if _, err := manifest.DecodeMetapage(valid[:length], 0); !errors.Is(err, dberrors.ErrCorruption) {
			t.Fatalf("prefix length %d error = %v, want corruption", length, err)
		}
	}
}

func FuzzDecodeMetapage(f *testing.F) {
	valid, _ := manifest.EncodeMetapage(0, 1, manifest.Meta{FileID: testFileID(), PageSize: page.Size})
	f.Add(valid, uint8(0))
	f.Add([]byte{}, uint8(0))
	f.Fuzz(func(t *testing.T, data []byte, slot uint8) {
		_, _ = manifest.DecodeMetapage(data, page.ID(slot%2))
	})
}
