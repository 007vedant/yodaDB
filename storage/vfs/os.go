package vfs

import (
	"fmt"
	"os"

	"yodadb/dberrors"
)

// OS implements VFS with operating-system files.
type OS struct{}

// Open opens an operating-system file with mode 0600 when it is created.
func (OS) Open(path string, options OpenOptions) (File, error) {
	if options.ReadOnly && (options.Create || options.Exclusive || options.Truncate) {
		return nil, dberrors.New(dberrors.CodeConstraint, "vfs.open", "read-only open cannot create, exclusively create, or truncate")
	}
	if options.Exclusive && !options.Create {
		return nil, dberrors.New(dberrors.CodeConstraint, "vfs.open", "exclusive open requires create")
	}
	flags := os.O_RDWR
	if options.ReadOnly {
		flags = os.O_RDONLY
	}
	if options.Create {
		flags |= os.O_CREATE
	}
	if options.Exclusive {
		flags |= os.O_EXCL
	}
	if options.Truncate {
		flags |= os.O_TRUNC
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, wrapIO(err, "vfs.open", path)
	}
	return &osFile{file: file, path: path}, nil
}

type osFile struct {
	file *os.File
	path string
}

func (f *osFile) ReadAt(buffer []byte, offset int64) (int, error) {
	count, err := f.file.ReadAt(buffer, offset)
	if err != nil {
		return count, wrapIO(err, "vfs.read_at", f.path)
	}
	return count, nil
}

func (f *osFile) WriteAt(buffer []byte, offset int64) (int, error) {
	count, err := f.file.WriteAt(buffer, offset)
	if err != nil {
		return count, wrapIO(err, "vfs.write_at", f.path)
	}
	return count, nil
}

func (f *osFile) Sync() error {
	return wrapIO(f.file.Sync(), "vfs.sync", f.path)
}

func (f *osFile) Truncate(size int64) error {
	if size < 0 {
		return dberrors.New(dberrors.CodeConstraint, "vfs.truncate", "size cannot be negative")
	}
	return wrapIO(f.file.Truncate(size), "vfs.truncate", f.path)
}

func (f *osFile) Size() (int64, error) {
	info, err := f.file.Stat()
	if err != nil {
		return 0, wrapIO(err, "vfs.size", f.path)
	}
	return info.Size(), nil
}

func (f *osFile) Lock(mode LockMode) error {
	if mode != LockShared && mode != LockExclusive {
		return dberrors.New(dberrors.CodeConstraint, "vfs.lock", fmt.Sprintf("invalid lock mode %d", mode))
	}
	if err := lockFile(f.file, mode); err != nil {
		if isLockConflict(err) {
			return dberrors.Wrap(err, dberrors.CodeConflict, "vfs.lock", fmt.Sprintf("file %q is already locked", f.path))
		}
		return wrapIO(err, "vfs.lock", f.path)
	}
	return nil
}

func (f *osFile) Unlock() error {
	return wrapIO(unlockFile(f.file), "vfs.unlock", f.path)
}

func (f *osFile) Close() error {
	return wrapIO(f.file.Close(), "vfs.close", f.path)
}

func (f *osFile) Path() string { return f.path }

func wrapIO(err error, operation, path string) error {
	if err == nil {
		return nil
	}
	return dberrors.Wrap(err, dberrors.CodeIO, operation, fmt.Sprintf("file %q", path))
}
