// Package dberrors defines error categories shared across yodaDB boundaries.
//
// Subsystems retain ownership of their detailed error types. This package only
// provides stable categories and contextual wrapping for callers that need to
// inspect errors with errors.Is and errors.As.
package dberrors

import (
	"errors"
	"strings"
)

// Code identifies a stable category of database error.
type Code string

const (
	// CodeUnknown identifies an error with no more specific category.
	CodeUnknown Code = "unknown"
	// CodeSyntax identifies invalid SQL syntax.
	CodeSyntax Code = "syntax"
	// CodeBind identifies name resolution and binding failures.
	CodeBind Code = "bind"
	// CodeType identifies invalid or incompatible value types.
	CodeType Code = "type"
	// CodeConstraint identifies constraint violations.
	CodeConstraint Code = "constraint"
	// CodeConflict identifies transaction validation conflicts.
	CodeConflict Code = "conflict"
	// CodeTransactionClosed identifies operations on a finished transaction.
	CodeTransactionClosed Code = "transaction_closed"
	// CodeCancelled identifies cancellation or timeout.
	CodeCancelled Code = "cancelled"
	// CodeUnsupported identifies a deliberately unsupported feature.
	CodeUnsupported Code = "unsupported"
	// CodeIO identifies an underlying input/output failure.
	CodeIO Code = "io"
	// CodeCorruption identifies invalid persisted data or structure.
	CodeCorruption Code = "corruption"
	// CodeResourceLimit identifies a configured resource limit violation.
	CodeResourceLimit Code = "resource_limit"
)

type categoryError Code

func (e categoryError) Error() string {
	return string(e)
}

// Category sentinels support errors.Is without discarding detailed context.
var (
	ErrUnknown           error = categoryError(CodeUnknown)
	ErrSyntax            error = categoryError(CodeSyntax)
	ErrBind              error = categoryError(CodeBind)
	ErrType              error = categoryError(CodeType)
	ErrConstraint        error = categoryError(CodeConstraint)
	ErrConflict          error = categoryError(CodeConflict)
	ErrTransactionClosed error = categoryError(CodeTransactionClosed)
	ErrCancelled         error = categoryError(CodeCancelled)
	ErrUnsupported       error = categoryError(CodeUnsupported)
	ErrIO                error = categoryError(CodeIO)
	ErrCorruption        error = categoryError(CodeCorruption)
	ErrResourceLimit     error = categoryError(CodeResourceLimit)
)

// Error adds a stable category and operation context to an error.
type Error struct {
	Code    Code
	Op      string
	Message string
	Err     error
}

// New constructs a categorized error without an underlying cause.
func New(code Code, op, message string) *Error {
	return &Error{Code: code, Op: op, Message: message}
}

// Wrap constructs a categorized error while preserving cause for errors.Is and
// errors.As. A nil cause is allowed and behaves like New.
func Wrap(cause error, code Code, op, message string) *Error {
	return &Error{Code: code, Op: op, Message: message, Err: cause}
}

// Error implements error.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}

	parts := make([]string, 0, 4)
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Code != "" {
		parts = append(parts, string(e.Code))
	}
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	if len(parts) == 0 {
		return "database error"
	}
	return strings.Join(parts, ": ")
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is matches another categorized Error by Code.
func (e *Error) Is(target error) bool {
	if e == nil {
		return false
	}
	switch other := target.(type) {
	case categoryError:
		return Code(other) != "" && e.Code == Code(other)
	case *Error:
		return other != nil && other.Code != "" && e.Code == other.Code
	default:
		return false
	}
}

// CodeOf returns the first yodaDB error category in err's unwrap chain. It
// returns CodeUnknown when no categorized error is present.
func CodeOf(err error) Code {
	var databaseError *Error
	if errors.As(err, &databaseError) {
		return databaseError.Code
	}
	var category categoryError
	if errors.As(err, &category) {
		return Code(category)
	}
	return CodeUnknown
}
