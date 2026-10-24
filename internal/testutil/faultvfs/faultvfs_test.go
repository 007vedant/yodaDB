package faultvfs_test

import (
	"context"
	"errors"
	"io"
	stdfs "io/fs"
	"slices"
	"sync"
	"testing"

	"yodadb/dberrors"
	"yodadb/internal/testutil/faultvfs"
	"yodadb/storage/manifest"
	"yodadb/storage/vfs"
)

func TestUnsyncedWritesDisappearAndSyncedWritesSurviveCrash(t *testing.T) {
	fs := newFS(t, faultvfs.Config{})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})

	write(t, file, []byte("old"), 0)
	if err := file.Sync(); err != nil {
		t.Fatalf("Sync old state: %v", err)
	}
	write(t, file, []byte("new"), 0)
	if got := string(fs.Visible("database")); got != "new" {
		t.Fatalf("Visible before crash = %q, want new", got)
	}
	if got := string(fs.Durable("database")); got != "old" {
		t.Fatalf("Durable before crash = %q, want old", got)
	}

	fs.Crash()
	if got := string(fs.Visible("database")); got != "old" {
		t.Fatalf("Visible after crash = %q, want old", got)
	}
	if _, err := file.Size(); !errors.Is(err, faultvfs.ErrCrash) {
		t.Fatalf("stale handle Size error = %v, want ErrCrash", err)
	}

	reopened := openFile(t, fs, "database", vfs.OpenOptions{})
	buffer := make([]byte, 3)
	if _, err := reopened.ReadAt(buffer, 0); err != nil {
		t.Fatalf("ReadAt after reopen: %v", err)
	}
	if got := string(buffer); got != "old" {
		t.Fatalf("ReadAt after reopen = %q, want old", got)
	}

	wantKinds := []faultvfs.Kind{
		faultvfs.Open, faultvfs.Write, faultvfs.Sync, faultvfs.Write,
		faultvfs.Crash, faultvfs.Size, faultvfs.Open, faultvfs.Read,
	}
	operations := fs.Operations()
	gotKinds := make([]faultvfs.Kind, len(operations))
	for index, operation := range operations {
		gotKinds[index] = operation.Kind
		if operation.Sequence != uint64(index+1) {
			t.Fatalf("operation %d sequence = %d, want %d", index, operation.Sequence, index+1)
		}
	}
	if !slices.Equal(gotKinds, wantKinds) {
		t.Fatalf("operation kinds = %v, want %v", gotKinds, wantKinds)
	}
}

func TestFailNthOperation(t *testing.T) {
	tests := []struct {
		name string
		kind faultvfs.Kind
		run  func(t *testing.T, file vfs.File)
	}{
		{
			name: "read",
			kind: faultvfs.Read,
			run: func(t *testing.T, file vfs.File) {
				_, err := file.ReadAt(make([]byte, 1), 0)
				assertIO(t, err)
			},
		},
		{
			name: "write",
			kind: faultvfs.Write,
			run: func(t *testing.T, file vfs.File) {
				_, err := file.WriteAt([]byte("x"), 0)
				assertIO(t, err)
			},
		},
		{
			name: "sync",
			kind: faultvfs.Sync,
			run: func(t *testing.T, file vfs.File) {
				assertIO(t, file.Sync())
			},
		},
		{
			name: "truncate",
			kind: faultvfs.Truncate,
			run: func(t *testing.T, file vfs.File) {
				assertIO(t, file.Truncate(0))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fs := newFS(t, faultvfs.Config{Faults: []faultvfs.Fault{{Kind: test.kind, At: 1, Action: faultvfs.Fail}}})
			file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})
			test.run(t, file)
			operations := fs.Operations()
			last := operations[len(operations)-1]
			if last.Kind != test.kind || last.Occurrence != 1 || last.Err == nil {
				t.Fatalf("last operation = %+v, want failed first %s", last, test.kind)
			}
		})
	}
}

func TestShortReadAndWriteExposeOnlyPrefix(t *testing.T) {
	fs := newFS(t, faultvfs.Config{Faults: []faultvfs.Fault{
		{Kind: faultvfs.Write, At: 1, Action: faultvfs.Short, Bytes: 3},
		{Kind: faultvfs.Read, At: 1, Action: faultvfs.Short, Bytes: 2},
	}})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})

	count, err := file.WriteAt([]byte("abcdef"), 0)
	if count != 3 || !errors.Is(err, io.ErrShortWrite) || dberrors.CodeOf(err) != dberrors.CodeIO {
		t.Fatalf("short WriteAt = (%d, %v), want (3, wrapped ErrShortWrite)", count, err)
	}
	buffer := make([]byte, 3)
	count, err = file.ReadAt(buffer, 0)
	if count != 2 || !errors.Is(err, io.ErrUnexpectedEOF) || dberrors.CodeOf(err) != dberrors.CodeIO {
		t.Fatalf("short ReadAt = (%d, %v), want (2, wrapped ErrUnexpectedEOF)", count, err)
	}
	if got := string(buffer[:count]); got != "ab" {
		t.Fatalf("short ReadAt prefix = %q, want ab", got)
	}
	if got := string(fs.Visible("database")); got != "abc" {
		t.Fatalf("visible bytes = %q, want abc", got)
	}
}

func TestTornWriteReturnsSuccessButPersistsOnlyPrefix(t *testing.T) {
	fs := newFS(t, faultvfs.Config{Faults: []faultvfs.Fault{{
		Kind: faultvfs.Write, At: 1, Action: faultvfs.Tear, Bytes: 3,
	}}})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})

	count, err := file.WriteAt([]byte("abcdef"), 0)
	if count != 6 || err != nil {
		t.Fatalf("torn WriteAt = (%d, %v), want (6, nil)", count, err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := string(fs.Visible("database")); got != "abcdef" {
		t.Fatalf("visible before crash = %q, want abcdef", got)
	}
	fs.Crash()
	if got := string(fs.Visible("database")); got != "abc" {
		t.Fatalf("visible after crash = %q, want abc", got)
	}
}

func TestZeroByteTearDoesNotExtendDurableFile(t *testing.T) {
	fs := newFS(t, faultvfs.Config{Faults: []faultvfs.Fault{{
		Kind: faultvfs.Write, At: 1, Action: faultvfs.Tear, Bytes: 0,
	}}})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})
	write(t, file, []byte("visible"), 8)
	if err := file.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := fs.Durable("database"); len(got) != 0 {
		t.Fatalf("durable bytes = %v, want empty", got)
	}
}

func TestSyncCanPersistPendingMutationsOutOfOrder(t *testing.T) {
	fs := newFS(t, faultvfs.Config{SyncBehaviors: []faultvfs.SyncBehavior{{
		At: 1, Mode: faultvfs.SyncSelected, MutationOrder: []int{2},
	}}})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})
	write(t, file, []byte("AAAA"), 0)
	write(t, file, []byte("BBBB"), 4)
	if err := file.Sync(); err != nil {
		t.Fatalf("Sync selected mutation: %v", err)
	}
	if got, want := fs.Durable("database"), []byte{0, 0, 0, 0, 'B', 'B', 'B', 'B'}; !slices.Equal(got, want) {
		t.Fatalf("durable bytes = %v, want %v", got, want)
	}

	fs.Crash()
	if got, want := fs.Visible("database"), []byte{0, 0, 0, 0, 'B', 'B', 'B', 'B'}; !slices.Equal(got, want) {
		t.Fatalf("visible bytes after crash = %v, want %v", got, want)
	}
}

func TestSuccessfulSyncMayPersistNothing(t *testing.T) {
	fs := newFS(t, faultvfs.Config{SyncBehaviors: []faultvfs.SyncBehavior{{At: 1, Mode: faultvfs.SyncNone}}})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})
	write(t, file, []byte("unsynced"), 0)
	if err := file.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	fs.Crash()
	if got := fs.Visible("database"); len(got) != 0 {
		t.Fatalf("visible after crash = %q, want empty", got)
	}
}

func TestAutomaticCrashAtOperationBoundary(t *testing.T) {
	fs := newFS(t, faultvfs.Config{CrashAfter: 2})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})
	count, err := file.WriteAt([]byte("lost"), 0)
	if count != 4 || !errors.Is(err, faultvfs.ErrCrash) {
		t.Fatalf("crashing WriteAt = (%d, %v), want (4, ErrCrash)", count, err)
	}
	if got := fs.Visible("database"); len(got) != 0 {
		t.Fatalf("visible after automatic crash = %q, want empty", got)
	}
	if _, err := file.Size(); !errors.Is(err, faultvfs.ErrCrash) {
		t.Fatalf("old handle Size error = %v, want ErrCrash", err)
	}
	operations := fs.Operations()
	if len(operations) < 3 || operations[1].Kind != faultvfs.Write || operations[2].Kind != faultvfs.Crash {
		t.Fatalf("operations = %+v, want write then crash", operations)
	}
}

func TestLockingAndOpenOptions(t *testing.T) {
	fs := newFS(t, faultvfs.Config{})
	first := openFile(t, fs, "database", vfs.OpenOptions{Create: true, Exclusive: true})
	second := openFile(t, fs, "database", vfs.OpenOptions{})
	if err := first.Lock(vfs.LockShared); err != nil {
		t.Fatalf("first shared lock: %v", err)
	}
	if err := second.Lock(vfs.LockExclusive); dberrors.CodeOf(err) != dberrors.CodeConflict {
		t.Fatalf("second exclusive lock error = %v, want conflict", err)
	}
	if err := first.Unlock(); err != nil {
		t.Fatalf("first unlock: %v", err)
	}
	if err := second.Lock(vfs.LockExclusive); err != nil {
		t.Fatalf("second exclusive lock: %v", err)
	}
	if _, err := fs.Open("database", vfs.OpenOptions{Create: true, Exclusive: true}); !errors.Is(err, stdfs.ErrExist) {
		t.Fatalf("exclusive create error = %v, want ErrExist", err)
	}

	readOnly := openFile(t, fs, "database", vfs.OpenOptions{ReadOnly: true})
	if _, err := readOnly.WriteAt([]byte("x"), 0); dberrors.CodeOf(err) != dberrors.CodeConstraint {
		t.Fatalf("read-only WriteAt error = %v, want constraint", err)
	}
}

func TestConcurrentAccessIsRaceSafe(t *testing.T) {
	fs := newFS(t, faultvfs.Config{})
	file := openFile(t, fs, "database", vfs.OpenOptions{Create: true})
	const workers = 16
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := range workers {
		go func() {
			defer wait.Done()
			if _, err := file.WriteAt([]byte{byte(worker + 1)}, int64(worker)); err != nil {
				t.Errorf("WriteAt worker %d: %v", worker, err)
			}
		}()
	}
	wait.Wait()
	if err := file.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if got := len(fs.Durable("database")); got != workers {
		t.Fatalf("durable length = %d, want %d", got, workers)
	}
}

func TestInvalidSchedulesAreRejected(t *testing.T) {
	tests := []faultvfs.Config{
		{Faults: []faultvfs.Fault{{Kind: faultvfs.Sync, At: 1, Action: faultvfs.Short}}},
		{Faults: []faultvfs.Fault{{Kind: faultvfs.Read, At: 0, Action: faultvfs.Fail}}},
		{SyncBehaviors: []faultvfs.SyncBehavior{{At: 0, Mode: faultvfs.SyncAll}}},
		{SyncBehaviors: []faultvfs.SyncBehavior{{At: 1, Mode: faultvfs.SyncNone, MutationOrder: []int{1}}}},
	}
	for index, config := range tests {
		if _, err := faultvfs.New(config); dberrors.CodeOf(err) != dberrors.CodeConstraint {
			t.Errorf("New invalid config %d error = %v, want constraint", index, err)
		}
	}
}

func TestManifestDatabaseCanCreateAndReopen(t *testing.T) {
	fs := newFS(t, faultvfs.Config{})
	database, err := manifest.CreateWithVFS(context.Background(), fs, "database", manifest.Options{})
	if err != nil {
		t.Fatalf("CreateWithVFS: %v", err)
	}
	want := database.Metadata()
	if err := database.Close(); err != nil {
		t.Fatalf("Close created database: %v", err)
	}
	fs.Crash()

	reopened, err := manifest.OpenWithVFS(context.Background(), fs, "database", manifest.Options{})
	if err != nil {
		t.Fatalf("OpenWithVFS: %v", err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close reopened database: %v", err)
		}
	}()
	if got := reopened.Metadata(); got != want {
		t.Fatalf("reopened metadata = %+v, want %+v", got, want)
	}
}

func newFS(t *testing.T, config faultvfs.Config) *faultvfs.FS {
	t.Helper()
	fs, err := faultvfs.New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fs
}

func openFile(t *testing.T, fs *faultvfs.FS, path string, options vfs.OpenOptions) vfs.File {
	t.Helper()
	file, err := fs.Open(path, options)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	return file
}

func write(t *testing.T, file vfs.File, data []byte, offset int64) {
	t.Helper()
	count, err := file.WriteAt(data, offset)
	if err != nil || count != len(data) {
		t.Fatalf("WriteAt = (%d, %v), want (%d, nil)", count, err, len(data))
	}
}

func assertIO(t *testing.T, err error) {
	t.Helper()
	if dberrors.CodeOf(err) != dberrors.CodeIO {
		t.Fatalf("error = %v, want I/O code", err)
	}
}
