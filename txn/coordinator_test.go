package txn_test

import (
	"errors"
	"testing"

	"yodadb/dberrors"
	"yodadb/txn"
)

func TestCoordinatorValidatesEveryCommitSinceBase(t *testing.T) {
	row1, _ := txn.NewRowResource(1, []byte("row-1"))
	row2, _ := txn.NewRowResource(1, []byte("row-2"))
	row3, _ := txn.NewRowResource(1, []byte("row-3"))
	coordinator := txn.NewCoordinator(10)

	var first, second txn.Footprint
	_ = first.RecordWrite(row1)
	_ = second.RecordWrite(row2)
	if err := coordinator.RecordCommitted(11, &first); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.RecordCommitted(12, &second); err != nil {
		t.Fatal(err)
	}

	var candidate txn.Footprint
	_ = candidate.RecordWrite(row2)
	_ = candidate.RecordWrite(row3)
	validation, err := coordinator.Validate(10, &candidate)
	if err != nil {
		t.Fatal(err)
	}
	if validation.LatestTxnID != 12 || len(validation.Conflicts) != 1 || validation.Conflicts[0] != row2 {
		t.Fatalf("validation=%+v, want row-2 conflict through transaction 12", validation)
	}

	validation, err = coordinator.Validate(11, &candidate)
	if err != nil || len(validation.Conflicts) != 1 || validation.Conflicts[0] != row2 {
		t.Fatalf("validation from 11=%+v error=%v", validation, err)
	}
	validation, err = coordinator.Validate(12, &candidate)
	if err != nil || len(validation.Conflicts) != 0 {
		t.Fatalf("validation at latest=%+v error=%v", validation, err)
	}
}

func TestCoordinatorRejectsUnavailableAndNonSequentialHistory(t *testing.T) {
	coordinator := txn.NewCoordinator(5)
	if _, err := coordinator.Validate(4, &txn.Footprint{}); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("old-base validation error=%v, want conflict", err)
	}
	if _, err := coordinator.Validate(6, &txn.Footprint{}); !errors.Is(err, dberrors.ErrConstraint) {
		t.Fatalf("future-base validation error=%v, want constraint", err)
	}
	if err := coordinator.RecordCommitted(7, &txn.Footprint{}); !errors.Is(err, dberrors.ErrConflict) {
		t.Fatalf("non-sequential record error=%v, want conflict", err)
	}
}

func TestFootprintCloneAndMergeAreIndependent(t *testing.T) {
	row1, _ := txn.NewRowResource(1, []byte("row-1"))
	row2, _ := txn.NewRowResource(1, []byte("row-2"))
	var original txn.Footprint
	_ = original.RecordWrite(row1)
	cloned := original.Clone()
	_ = cloned.RecordWrite(row2)
	if len(original.Writes()) != 1 {
		t.Fatalf("original writes=%v, want independent clone", original.Writes())
	}
	original.Merge(&cloned)
	if len(original.Writes()) != 2 {
		t.Fatalf("merged writes=%v, want two", original.Writes())
	}
}
