package manifest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"yodadb/dberrors"
	"yodadb/internal/testutil/faultvfs"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
)

const crashDatabasePath = "crash-matrix.yoda"

type committedState struct {
	txnID uint64
	root  page.ID
	value string
}

func TestCommitCrashAfterEveryVFSOperation(t *testing.T) {
	baselineFS, baseline, _, newState := prepareCommit(t, faultvfs.Config{})
	if _, err := baseline.Publish(context.Background(), nextManifest(newState)); err != nil {
		t.Fatalf("baseline Publish: %v", err)
	}
	operations := baselineFS.Operations()
	wantKinds := []faultvfs.Kind{faultvfs.Write, faultvfs.Sync, faultvfs.Write, faultvfs.Sync}
	gotKinds := make([]faultvfs.Kind, len(operations))
	for index, operation := range operations {
		gotKinds[index] = operation.Kind
	}
	if !slices.Equal(gotKinds, wantKinds) {
		t.Fatalf("baseline commit operations = %v, want %v", gotKinds, wantKinds)
	}
	baselineFS.Crash()
	assertRecoveredState(t, baselineFS, newState)

	for boundary := 1; boundary <= len(operations); boundary++ {
		t.Run(fmt.Sprintf("after_operation_%d_%s", boundary, operations[boundary-1].Kind), func(t *testing.T) {
			filesystem, database, oldState, newState := prepareCommit(t, faultvfs.Config{CrashAfter: uint64(boundary)})
			if _, err := database.Publish(context.Background(), nextManifest(newState)); !errors.Is(err, faultvfs.ErrCrash) {
				t.Fatalf("Publish error = %v, want simulated crash", err)
			}
			if !logContainsCrashAfter(filesystem.Operations(), uint64(boundary)) {
				t.Fatalf("operation log does not contain crash after boundary %d: %+v", boundary, filesystem.Operations())
			}
			wantAfterCrash := []faultvfs.Kind(nil)
			if boundary == 1 {
				// Pager append makes one best-effort truncate after a failed write.
				wantAfterCrash = []faultvfs.Kind{faultvfs.Truncate}
			}
			if got := kindsAfterCrash(filesystem.Operations()); !slices.Equal(got, wantAfterCrash) {
				t.Fatalf("operations after crash = %v, want %v", got, wantAfterCrash)
			}
			want := oldState
			if boundary == len(operations) {
				want = newState
			}
			assertRecoveredState(t, filesystem, want)
		})
	}
}

func TestCommitWriteAndSyncFailuresRecoverOldState(t *testing.T) {
	tests := []struct {
		name   string
		config faultvfs.Config
	}{
		{name: "manifest_write", config: fail(faultvfs.Write, 1)},
		{name: "data_sync", config: fail(faultvfs.Sync, 1)},
		{name: "metapage_write", config: fail(faultvfs.Write, 2)},
		{name: "metapage_sync", config: fail(faultvfs.Sync, 2)},
		{name: "short_manifest_write", config: shortWrite(1, page.Size/2)},
		{name: "short_metapage_write", config: shortWrite(2, page.Size/2)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filesystem, database, oldState, newState := prepareCommit(t, test.config)
			if _, err := database.Publish(context.Background(), nextManifest(newState)); dberrors.CodeOf(err) != dberrors.CodeIO {
				t.Fatalf("Publish error = %v, want I/O error", err)
			}
			filesystem.Crash()
			assertRecoveredState(t, filesystem, oldState)
		})
	}
}

func TestCommitTornPageWritesRecoverExactlyOldOrNewState(t *testing.T) {
	for _, occurrence := range []int{1, 2} {
		for _, durablePrefix := range []int{0, 1, page.HeaderSize, page.Size / 2, page.Size - 1} {
			name := fmt.Sprintf("write_%d_prefix_%d", occurrence, durablePrefix)
			t.Run(name, func(t *testing.T) {
				config := faultvfs.Config{Faults: []faultvfs.Fault{{
					Kind: faultvfs.Write, At: occurrence, Action: faultvfs.Tear, Bytes: durablePrefix,
				}}}
				filesystem, database, oldState, newState := prepareCommit(t, config)
				if _, err := database.Publish(context.Background(), nextManifest(newState)); err != nil {
					t.Fatalf("Publish returned error for acknowledged torn write: %v", err)
				}
				filesystem.Crash()
				assertRecoveredOldOrNew(t, filesystem, oldState, newState)
			})
		}
	}
}

func TestCommitReorderedSyncVisibilityIsOldOrNew(t *testing.T) {
	tests := []struct {
		name   string
		config faultvfs.Config
		new    bool
	}{
		{
			name: "only_metapage_durable",
			config: faultvfs.Config{SyncBehaviors: []faultvfs.SyncBehavior{
				{At: 1, Mode: faultvfs.SyncNone},
				{At: 2, Mode: faultvfs.SyncSelected, MutationOrder: []int{3}},
			}},
		},
		{
			name: "manifest_then_metapage_without_root",
			config: faultvfs.Config{SyncBehaviors: []faultvfs.SyncBehavior{
				{At: 1, Mode: faultvfs.SyncSelected, MutationOrder: []int{2}},
				{At: 2, Mode: faultvfs.SyncSelected, MutationOrder: []int{2}},
			}},
		},
		{
			name: "root_then_metapage_without_manifest",
			config: faultvfs.Config{SyncBehaviors: []faultvfs.SyncBehavior{
				{At: 1, Mode: faultvfs.SyncSelected, MutationOrder: []int{1}},
				{At: 2, Mode: faultvfs.SyncSelected, MutationOrder: []int{2}},
			}},
		},
		{
			name: "all_pending_delayed_until_metapage_sync",
			config: faultvfs.Config{SyncBehaviors: []faultvfs.SyncBehavior{
				{At: 1, Mode: faultvfs.SyncNone},
			}},
			new: true,
		},
		{
			name: "root_and_manifest_persisted_in_reverse_order",
			config: faultvfs.Config{SyncBehaviors: []faultvfs.SyncBehavior{
				{At: 1, Mode: faultvfs.SyncSelected, MutationOrder: []int{2, 1}},
			}},
			new: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filesystem, database, oldState, newState := prepareCommit(t, test.config)
			if _, err := database.Publish(context.Background(), nextManifest(newState)); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			filesystem.Crash()
			want := oldState
			if test.new {
				want = newState
			}
			assertRecoveredState(t, filesystem, want)
		})
	}
}

func prepareCommit(t *testing.T, config faultvfs.Config) (*faultvfs.FS, *manifest.Database, committedState, committedState) {
	t.Helper()
	filesystem, err := faultvfs.New(faultvfs.Config{})
	if err != nil {
		t.Fatalf("faultvfs.New: %v", err)
	}
	database, err := manifest.CreateWithVFS(context.Background(), filesystem, crashDatabasePath, manifest.Options{})
	if err != nil {
		t.Fatalf("CreateWithVFS: %v", err)
	}

	oldState := committedState{txnID: 1, value: "old"}
	oldState.root = appendLeaf(t, database, oldState.txnID, oldState.value)
	if _, err := database.Publish(context.Background(), nextManifest(oldState)); err != nil {
		t.Fatalf("publish old state: %v", err)
	}

	newState := committedState{txnID: 2, value: "new"}
	newState.root = appendLeaf(t, database, newState.txnID, newState.value)
	if err := filesystem.Arm(config); err != nil {
		t.Fatalf("Arm: %v", err)
	}
	return filesystem, database, oldState, newState
}

func appendLeaf(t *testing.T, database *manifest.Database, txnID uint64, value string) page.ID {
	t.Helper()
	builder := page.NewLeafBuilder(0)
	if err := builder.Insert(0, []byte("key"), []byte(value)); err != nil {
		t.Fatalf("leaf Insert: %v", err)
	}
	payload, err := builder.Encode()
	if err != nil {
		t.Fatalf("leaf Encode: %v", err)
	}
	root, err := database.Append(page.TypeBTreeLeaf, txnID, payload)
	if err != nil {
		t.Fatalf("Append leaf: %v", err)
	}
	return root
}

func nextManifest(state committedState) manifest.Manifest {
	return manifest.Manifest{
		TxnID:       state.txnID,
		ParentTxnID: state.txnID - 1,
		TableRoots:  map[uint64]page.ID{1: state.root},
		IndexRoots:  map[uint64]page.ID{},
	}
}

func assertRecoveredState(t *testing.T, filesystem *faultvfs.FS, want committedState) {
	t.Helper()
	got := recoverState(t, filesystem)
	if got != want {
		t.Fatalf("recovered state = %+v, want %+v", got, want)
	}
}

func assertRecoveredOldOrNew(t *testing.T, filesystem *faultvfs.FS, oldState, newState committedState) {
	t.Helper()
	got := recoverState(t, filesystem)
	if got != oldState && got != newState {
		t.Fatalf("recovered state = %+v, want exactly old %+v or new %+v", got, oldState, newState)
	}
}

func recoverState(t *testing.T, filesystem *faultvfs.FS) committedState {
	t.Helper()
	database, err := manifest.OpenWithVFS(context.Background(), filesystem, crashDatabasePath, manifest.Options{})
	if err != nil {
		t.Fatalf("OpenWithVFS after crash: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close recovered database: %v", err)
		}
	}()
	record, found, err := database.LatestManifest()
	if err != nil || !found {
		t.Fatalf("LatestManifest = (%+v, %t, %v), want committed state", record, found, err)
	}
	root := record.TableRoots[1]
	if record.ParentTxnID != record.TxnID-1 || root == 0 {
		t.Fatalf("recovered manifest has inconsistent identity: %+v", record)
	}
	if len(record.TableRoots) != 1 || len(record.IndexRoots) != 0 {
		t.Fatalf("recovered root maps = tables %v indexes %v", record.TableRoots, record.IndexRoots)
	}
	stored, err := database.ReadPage(root)
	if err != nil {
		t.Fatalf("ReadPage(%d): %v", root, err)
	}
	leaf, err := page.DecodeLeaf(stored.Payload)
	if err != nil {
		t.Fatalf("DecodeLeaf(%d): %v", root, err)
	}
	if len(leaf.Entries) != 1 || string(leaf.Entries[0].Key) != "key" {
		t.Fatalf("recovered leaf = %+v, want one key %q", leaf, "key")
	}
	return committedState{txnID: record.TxnID, root: root, value: string(leaf.Entries[0].Value)}
}

func logContainsCrashAfter(operations []faultvfs.Operation, boundary uint64) bool {
	for index, operation := range operations {
		if operation.Sequence == boundary && index+1 < len(operations) && operations[index+1].Kind == faultvfs.Crash {
			return true
		}
	}
	return false
}

func kindsAfterCrash(operations []faultvfs.Operation) []faultvfs.Kind {
	for index, operation := range operations {
		if operation.Kind == faultvfs.Crash {
			result := make([]faultvfs.Kind, 0, len(operations)-index-1)
			for _, later := range operations[index+1:] {
				result = append(result, later.Kind)
			}
			return result
		}
	}
	return nil
}

func fail(kind faultvfs.Kind, occurrence int) faultvfs.Config {
	return faultvfs.Config{Faults: []faultvfs.Fault{{Kind: kind, At: occurrence, Action: faultvfs.Fail}}}
}

func shortWrite(occurrence, bytes int) faultvfs.Config {
	return faultvfs.Config{Faults: []faultvfs.Fault{{Kind: faultvfs.Write, At: occurrence, Action: faultvfs.Short, Bytes: bytes}}}
}
