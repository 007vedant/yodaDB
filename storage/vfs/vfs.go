package vfs

import "io"

// LockMode identifies an advisory file-lock mode.
type LockMode uint8

const (
	// LockShared permits other shared lock holders.
	LockShared LockMode = iota + 1
	// LockExclusive excludes every other lock holder.
	LockExclusive
)

// OpenOptions controls how a VFS file is opened.
type OpenOptions struct {
	ReadOnly  bool
	Create    bool
	Exclusive bool
	Truncate  bool
}

// File is the positional I/O contract used by the pager and recovery code.
type File interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Truncate(size int64) error
	Size() (int64, error)
	Lock(mode LockMode) error
	Unlock() error
	Close() error
	Path() string
}

// VFS opens database files behind a replaceable test boundary.
type VFS interface {
	Open(path string, options OpenOptions) (File, error)
}
