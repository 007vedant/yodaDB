package dberrors_test

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"yodadb/dberrors"
)

func TestWrapPreservesCategoryAndCause(t *testing.T) {
	cause := io.ErrUnexpectedEOF
	err := dberrors.Wrap(cause, dberrors.CodeIO, "pager.read", "read page 7")

	if !errors.Is(err, dberrors.ErrIO) {
		t.Fatal("wrapped error does not match I/O category")
	}
	if !errors.Is(err, cause) {
		t.Fatal("wrapped error does not preserve its cause")
	}
	if errors.Is(err, dberrors.ErrCorruption) {
		t.Fatal("I/O error unexpectedly matches corruption category")
	}
	if got, want := err.Error(), "pager.read: io: read page 7: unexpected EOF"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestErrorSupportsErrorsAsThroughAdditionalContext(t *testing.T) {
	databaseError := dberrors.New(dberrors.CodeConstraint, "table.insert", "duplicate key")
	err := fmt.Errorf("execute statement: %w", databaseError)

	var got *dberrors.Error
	if !errors.As(err, &got) {
		t.Fatal("errors.As did not find categorized error")
	}
	if got != databaseError {
		t.Fatal("errors.As returned a different error")
	}
	if code := dberrors.CodeOf(err); code != dberrors.CodeConstraint {
		t.Fatalf("CodeOf() = %q, want %q", code, dberrors.CodeConstraint)
	}
}

func TestCodeOfUncategorizedError(t *testing.T) {
	if got := dberrors.CodeOf(errors.New("plain error")); got != dberrors.CodeUnknown {
		t.Fatalf("CodeOf() = %q, want %q", got, dberrors.CodeUnknown)
	}
}

func TestCodeOfCategorySentinel(t *testing.T) {
	if got := dberrors.CodeOf(dberrors.ErrUnsupported); got != dberrors.CodeUnsupported {
		t.Fatalf("CodeOf() = %q, want %q", got, dberrors.CodeUnsupported)
	}
}

func TestNilErrorFormattingIsSafe(t *testing.T) {
	var err *dberrors.Error
	if got, want := err.Error(), "<nil>"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}
