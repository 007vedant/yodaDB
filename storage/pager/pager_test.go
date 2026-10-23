package pager_test

import (
	"errors"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/page"
	"yodadb/storage/pager"
	"yodadb/storage/vfs"
)

func TestAppendCloseReopenAndDecodeIdentically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pager.yoda")
	file, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := pager.Open(file, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		pageType page.Type
		payload  []byte
	}{
		{page.TypeMeta, []byte("meta-a")},
		{page.TypeMeta, []byte("meta-b")},
		{page.TypeCatalog, []byte("catalog")},
	} {
		if _, err := first.Append(item.pageType, 1, item.payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := first.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopenedFile, err := (vfs.OS{}).Open(path, vfs.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := pager.Open(reopenedFile, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.PageCount() != 3 {
		t.Fatalf("PageCount = %d, want 3", reopened.PageCount())
	}
	handle, err := reopened.Read(2)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(handle.Page().Payload); got != "catalog" {
		t.Fatalf("page 2 payload = %q, want catalog", got)
	}
	handle.Release()
}

func TestCachePinsPreventEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.yoda")
	file, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pager.Open(file, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Append(page.TypeMeta, 0, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Append(page.TypeMeta, 0, nil); err != nil {
		t.Fatal(err)
	}
	first, err := p.Read(0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Read(1); !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("Read with pinned cache error = %v, want resource limit", err)
	}
	first.Release()
	second, err := p.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	stats := p.Stats()
	if stats.Entries != 1 || stats.Misses != 2 {
		t.Fatalf("cache stats = %+v, want one entry and two successful misses", stats)
	}
}

func TestPagerValidatesFileAndPageIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.yoda")
	file, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte{1}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := pager.Open(file, 1); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("Open misaligned file error = %v, want corruption", err)
	}
	_ = file.Close()

	identityPath := filepath.Join(t.TempDir(), "identity.yoda")
	identityFile, err := (vfs.OS{}).Open(identityPath, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := page.Encode(page.Header{Type: page.TypeMeta, PageID: 9}, nil)
	if _, err := identityFile.WriteAt(encoded, 0); err != nil {
		t.Fatal(err)
	}
	p, err := pager.Open(identityFile, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Read(0); !errors.Is(err, dberrors.ErrCorruption) {
		t.Fatalf("Read mismatched page ID error = %v, want corruption", err)
	}
}

func TestPagerEnforcesReservedMetapages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reserved.yoda")
	file, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	p, err := pager.Open(file, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Append(page.TypeCatalog, 0, nil); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("catalog at page 0 error = %v, want constraint", err)
	}
}
