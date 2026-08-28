package vfs_test

import (
	"errors"
	"io"
	"path/filepath"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/vfs"
)

func TestOSFilePositionalIOAndTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.yoda")
	file, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if count, err := file.WriteAt([]byte("YODA"), 8); err != nil || count != 4 {
		t.Fatalf("WriteAt = %d, %v; want 4, nil", count, err)
	}
	buffer := make([]byte, 4)
	if count, err := file.ReadAt(buffer, 8); err != nil || count != 4 || string(buffer) != "YODA" {
		t.Fatalf("ReadAt = %d, %v, %q; want 4, nil, YODA", count, err, buffer)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if size, err := file.Size(); err != nil || size != 12 {
		t.Fatalf("Size = %d, %v; want 12, nil", size, err)
	}
	if err := file.Truncate(4); err != nil {
		t.Fatal(err)
	}
	if size, err := file.Size(); err != nil || size != 4 {
		t.Fatalf("Size after truncate = %d, %v; want 4, nil", size, err)
	}
}

func TestOSFilePreservesShortReadCause(t *testing.T) {
	path := filepath.Join(t.TempDir(), "short.yoda")
	file, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	buffer := make([]byte, 1)
	_, err = file.ReadAt(buffer, 0)
	if !errors.Is(err, dberrors.ErrIO) || !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt error = %v, want I/O category preserving EOF", err)
	}
}

func TestOSOpenExclusiveAndAdvisoryLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.yoda")
	first, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true}); !errors.Is(err, dberrors.ErrIO) {
		t.Fatalf("exclusive reopen error = %v, want I/O category", err)
	}
	second, err := (vfs.OS{}).Open(path, vfs.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.Lock(vfs.LockExclusive); err != nil {
		t.Fatal(err)
	}
	defer first.Unlock()
	if err := second.Lock(vfs.LockShared); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("second lock error = %v, want conflict", err)
	}
}

func TestOSFileRejectsNegativeTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "negative.yoda")
	file, err := (vfs.OS{}).Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(-1); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("Truncate error = %v, want constraint", err)
	}
}

func TestOSRejectsContradictoryOpenOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "options.yoda")
	for _, options := range []vfs.OpenOptions{
		{ReadOnly: true, Create: true},
		{ReadOnly: true, Truncate: true},
		{Exclusive: true},
	} {
		if _, err := (vfs.OS{}).Open(path, options); !errors.Is(err, dberrors.ErrConstraint) {
			t.Fatalf("Open(%+v) error = %v, want constraint", options, err)
		}
	}
}
