package txn_test

import (
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/manifest"
	"yodadb/storage/page"
	"yodadb/txn"
)

func TestWriteLifecycleAndDefensiveRoots(t *testing.T) {
	base := manifest.Manifest{TxnID: 7, TableRoots: map[uint64]page.ID{1: 10}, IndexRoots: map[uint64]page.ID{2: 20}}
	write, err := txn.BeginWrite(base)
	if err != nil {
		t.Fatal(err)
	}
	working, err := write.Working()
	if err != nil {
		t.Fatal(err)
	}
	if write.State() != txn.Active || write.TxnID() != 8 || working.ParentTxnID != 7 {
		t.Fatalf("unexpected transaction: state=%v manifest=%+v", write.State(), working)
	}
	working.TableRoots[1] = 11
	if err := write.Advance(working); err != nil {
		t.Fatal(err)
	}
	working.TableRoots[1] = 99
	record, dirty, err := write.PrepareCommit()
	if err != nil || !dirty || record.TableRoots[1] != 11 {
		t.Fatalf("prepared=(%+v,%t,%v)", record, dirty, err)
	}
	if err := write.CommitSucceeded(); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Working(); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("Working after commit error=%v", err)
	}
}

func TestWriteRollbackAndReadOnlyCommit(t *testing.T) {
	write, err := txn.BeginWrite(manifest.Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	_, dirty, err := write.PrepareCommit()
	if err != nil || dirty {
		t.Fatalf("read-only prepare dirty=%t error=%v", dirty, err)
	}
	if err := write.CommitSucceeded(); err != nil {
		t.Fatal(err)
	}

	write, err = txn.BeginWrite(manifest.Manifest{})
	if err != nil {
		t.Fatal(err)
	}
	if err := write.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := write.Rollback(); !errors.Is(err, dberrors.ErrTransactionClosed) {
		t.Fatalf("second rollback error=%v", err)
	}
}

func TestBeginWriteRejectsExhaustedTransactionIDs(t *testing.T) {
	_, err := txn.BeginWrite(manifest.Manifest{TxnID: ^uint64(0)})
	if !errors.Is(err, dberrors.ErrResourceLimit) {
		t.Fatalf("BeginWrite error=%v, want resource limit", err)
	}
}
