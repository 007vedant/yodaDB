package faultvfs

import (
	"errors"
	"fmt"
	"io"
	stdfs "io/fs"
	"slices"
	"sync"

	"yodadb/dberrors"
	"yodadb/storage/vfs"
)

// ErrCrash identifies a simulated process or power-loss crash.
var ErrCrash = errors.New("simulated process crash")

// Kind identifies an operation in the deterministic operation log.
type Kind string

const (
	Open         Kind = "open"
	Read         Kind = "read"
	Write        Kind = "write"
	Sync         Kind = "sync"
	Truncate     Kind = "truncate"
	Size         Kind = "size"
	Lock         Kind = "lock"
	Unlock       Kind = "unlock"
	Close        Kind = "close"
	Crash        Kind = "crash"
	ProcessCrash Kind = "process_crash"
)

// Action describes how a matching operation is altered.
type Action uint8

const (
	Fail Action = iota + 1
	Short
	Tear
)

// Fault alters the At-th occurrence of Kind. Bytes is the successful prefix
// length for Short and the durable prefix length for Tear.
type Fault struct {
	Kind   Kind
	At     int
	Action Action
	Bytes  int
}

// SyncMode controls which pending mutations a successful sync makes durable.
type SyncMode uint8

const (
	SyncAll SyncMode = iota
	SyncNone
	SyncSelected
)

// SyncBehavior alters the At-th sync. MutationOrder contains one-based indexes
// into the pending mutation list and may deliberately reorder persistence.
type SyncBehavior struct {
	At            int
	Mode          SyncMode
	MutationOrder []int
}

// Config defines a deterministic fault schedule.
type Config struct {
	Faults        []Fault
	SyncBehaviors []SyncBehavior
	// CrashAfter discards unsynced bytes immediately after this global operation
	// sequence number. Zero disables automatic crash injection.
	CrashAfter uint64
	// ProcessCrashAfter invalidates process handles but retains kernel-visible
	// unsynced bytes after this global operation sequence number.
	ProcessCrashAfter uint64
}

// Operation records one VFS call without recording user data.
type Operation struct {
	Sequence   uint64
	Kind       Kind
	Occurrence int
	Path       string
	Offset     int64
	Requested  int
	Completed  int
	Err        error
}

type mutationKind uint8

const (
	writeMutation mutationKind = iota + 1
	truncateMutation
)

type mutation struct {
	kind         mutationKind
	offset       int64
	data         []byte
	size         int64
	durableBytes int
}

type fileState struct {
	durable   []byte
	visible   []byte
	pending   []mutation
	shared    map[uint64]struct{}
	exclusive uint64
}

// FS is a concurrency-safe, crashable in-memory implementation of vfs.VFS.
type FS struct {
	mu          sync.Mutex
	config      Config
	files       map[string]*fileState
	operations  []Operation
	occurrences map[Kind]int
	epoch       uint64
	nextHandle  uint64
	crashing    bool
}

// New constructs a fault VFS after validating the fault schedule.
func New(config Config) (*FS, error) {
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &FS{
		config:      cloneConfig(config),
		files:       make(map[string]*fileState),
		occurrences: make(map[Kind]int),
	}, nil
}

func cloneConfig(config Config) Config {
	config.Faults = slices.Clone(config.Faults)
	config.SyncBehaviors = slices.Clone(config.SyncBehaviors)
	for i := range config.SyncBehaviors {
		config.SyncBehaviors[i].MutationOrder = slices.Clone(config.SyncBehaviors[i].MutationOrder)
	}
	return config
}

func validateConfig(config Config) error {
	if config.CrashAfter != 0 && config.ProcessCrashAfter != 0 {
		return constraint("faultvfs.new", "power and process crash boundaries are mutually exclusive")
	}
	seenFault := make(map[[2]int64]struct{})
	for _, fault := range config.Faults {
		if fault.At <= 0 || fault.Bytes < 0 {
			return constraint("faultvfs.new", "fault occurrence must be positive and byte count nonnegative")
		}
		if fault.Kind != Read && fault.Kind != Write && fault.Kind != Sync && fault.Kind != Truncate {
			return constraint("faultvfs.new", fmt.Sprintf("operation %q cannot be faulted", fault.Kind))
		}
		if fault.Action != Fail && fault.Action != Short && fault.Action != Tear {
			return constraint("faultvfs.new", "unknown fault action")
		}
		if fault.Action == Short && fault.Kind != Read && fault.Kind != Write {
			return constraint("faultvfs.new", "short faults require a read or write")
		}
		if fault.Action == Tear && fault.Kind != Write {
			return constraint("faultvfs.new", "torn faults require a write")
		}
		key := [2]int64{int64(kindNumber(fault.Kind)), int64(fault.At)}
		if _, exists := seenFault[key]; exists {
			return constraint("faultvfs.new", "duplicate fault occurrence")
		}
		seenFault[key] = struct{}{}
	}
	seenSync := make(map[int]struct{})
	for _, behavior := range config.SyncBehaviors {
		if behavior.At <= 0 {
			return constraint("faultvfs.new", "sync occurrence must be positive")
		}
		if behavior.Mode != SyncAll && behavior.Mode != SyncNone && behavior.Mode != SyncSelected {
			return constraint("faultvfs.new", "unknown sync mode")
		}
		if behavior.Mode != SyncSelected && len(behavior.MutationOrder) != 0 {
			return constraint("faultvfs.new", "mutation order requires selected sync mode")
		}
		if _, exists := seenSync[behavior.At]; exists {
			return constraint("faultvfs.new", "duplicate sync behavior")
		}
		seenSync[behavior.At] = struct{}{}
	}
	return nil
}

func kindNumber(kind Kind) int {
	for index, candidate := range []Kind{Open, Read, Write, Sync, Truncate, Size, Lock, Unlock, Close, Crash, ProcessCrash} {
		if candidate == kind {
			return index + 1
		}
	}
	return 0
}

// Open opens an in-memory file using the same option constraints as vfs.OS.
func (fs *FS) Open(path string, options vfs.OpenOptions) (vfs.File, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if options.ReadOnly && (options.Create || options.Exclusive || options.Truncate) {
		return nil, fs.finishLocked(Operation{Kind: Open, Path: path}, constraint("faultvfs.open", "read-only open cannot create, exclusively create, or truncate"))
	}
	if options.Exclusive && !options.Create {
		return nil, fs.finishLocked(Operation{Kind: Open, Path: path}, constraint("faultvfs.open", "exclusive open requires create"))
	}
	state, exists := fs.files[path]
	if !exists && !options.Create {
		return nil, fs.finishLocked(Operation{Kind: Open, Path: path}, ioError(stdfs.ErrNotExist, "faultvfs.open", path))
	}
	if exists && options.Create && options.Exclusive {
		return nil, fs.finishLocked(Operation{Kind: Open, Path: path}, ioError(stdfs.ErrExist, "faultvfs.open", path))
	}
	if !exists {
		state = &fileState{shared: make(map[uint64]struct{})}
		fs.files[path] = state
	}
	if options.Truncate {
		state.visible = nil
		state.pending = append(state.pending, mutation{kind: truncateMutation})
	}
	fs.nextHandle++
	handle := &file{fs: fs, state: state, path: path, id: fs.nextHandle, epoch: fs.epoch, readOnly: options.ReadOnly}
	if err := fs.finishLocked(Operation{Kind: Open, Path: path}, nil); err != nil {
		return nil, err
	}
	return handle, nil
}

// Crash simulates power loss by discarding unsynced state. Existing handles
// become invalid and locks are released.
func (fs *FS) Crash() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.crashLocked(Crash, true, true)
}

// ProcessCrash invalidates handles and releases locks while retaining unsynced
// bytes as process-visible kernel state.
func (fs *FS) ProcessCrash() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.crashLocked(ProcessCrash, false, true)
}

// Operations returns an independent snapshot of the deterministic log.
func (fs *FS) Operations() []Operation {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return slices.Clone(fs.operations)
}

// ResetLog clears operation history and occurrence counters without changing files.
func (fs *FS) ResetLog() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.operations = nil
	clear(fs.occurrences)
}

// Arm replaces the fault schedule and resets operation numbering while
// preserving files, pending mutations, locks, and open handles. Tests use it
// immediately before the operation sequence under examination.
func (fs *FS) Arm(config Config) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.config = cloneConfig(config)
	fs.operations = nil
	clear(fs.occurrences)
	return nil
}

// Durable returns a copy of bytes that survive a crash.
func (fs *FS) Durable(path string) []byte {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if state := fs.files[path]; state != nil {
		return slices.Clone(state.durable)
	}
	return nil
}

// Visible returns a copy of bytes visible to the current process.
func (fs *FS) Visible(path string) []byte {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if state := fs.files[path]; state != nil {
		return slices.Clone(state.visible)
	}
	return nil
}

func (fs *FS) finishLocked(operation Operation, err error) error {
	fs.occurrences[operation.Kind]++
	operation.Occurrence = fs.occurrences[operation.Kind]
	operation.Sequence = uint64(len(fs.operations) + 1)
	operation.Err = err
	fs.operations = append(fs.operations, operation)
	if !fs.crashing && fs.config.CrashAfter == operation.Sequence {
		fs.crashLocked(Crash, true, true)
		return ioError(ErrCrash, "faultvfs.crash", operation.Path)
	}
	if !fs.crashing && fs.config.ProcessCrashAfter == operation.Sequence {
		fs.crashLocked(ProcessCrash, false, true)
		return ioError(ErrCrash, "faultvfs.process_crash", operation.Path)
	}
	return err
}

func (fs *FS) crashLocked(kind Kind, discardUnsynced, log bool) {
	fs.crashing = true
	for _, state := range fs.files {
		if discardUnsynced {
			state.visible = slices.Clone(state.durable)
			state.pending = nil
		}
		clear(state.shared)
		state.exclusive = 0
	}
	fs.epoch++
	if log {
		fs.occurrences[kind]++
		fs.operations = append(fs.operations, Operation{
			Sequence: uint64(len(fs.operations) + 1), Kind: kind, Occurrence: fs.occurrences[kind], Err: ErrCrash,
		})
	}
	fs.crashing = false
}

func (fs *FS) faultLocked(kind Kind) (Fault, bool) {
	next := fs.occurrences[kind] + 1
	for _, fault := range fs.config.Faults {
		if fault.Kind == kind && fault.At == next {
			return fault, true
		}
	}
	return Fault{}, false
}

func (fs *FS) syncBehaviorLocked() SyncBehavior {
	next := fs.occurrences[Sync] + 1
	for _, behavior := range fs.config.SyncBehaviors {
		if behavior.At == next {
			return behavior
		}
	}
	return SyncBehavior{Mode: SyncAll}
}

type file struct {
	fs       *FS
	state    *fileState
	path     string
	id       uint64
	epoch    uint64
	readOnly bool
	closed   bool
}

func (f *file) ReadAt(buffer []byte, offset int64) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Read, Path: f.path, Offset: offset, Requested: len(buffer)}
	if err := f.usableLocked("faultvfs.read_at"); err != nil {
		return 0, f.fs.finishLocked(op, err)
	}
	if offset < 0 {
		return 0, f.fs.finishLocked(op, constraint("faultvfs.read_at", "offset cannot be negative"))
	}
	if fault, ok := f.fs.faultLocked(Read); ok && fault.Action == Fail {
		return 0, f.fs.finishLocked(op, injected(Read))
	} else if ok && fault.Action == Short {
		limit := min(fault.Bytes, len(buffer))
		count := copyAt(buffer[:limit], f.state.visible, offset)
		op.Completed = count
		return count, f.fs.finishLocked(op, ioError(io.ErrUnexpectedEOF, "faultvfs.read_at", f.path))
	}
	count := copyAt(buffer, f.state.visible, offset)
	op.Completed = count
	if count != len(buffer) {
		return count, f.fs.finishLocked(op, ioError(io.EOF, "faultvfs.read_at", f.path))
	}
	return count, f.fs.finishLocked(op, nil)
}

func copyAt(destination, source []byte, offset int64) int {
	if offset >= int64(len(source)) {
		return 0
	}
	return copy(destination, source[int(offset):])
}

func (f *file) WriteAt(buffer []byte, offset int64) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Write, Path: f.path, Offset: offset, Requested: len(buffer)}
	if err := f.writableLocked("faultvfs.write_at"); err != nil {
		return 0, f.fs.finishLocked(op, err)
	}
	if offset < 0 {
		return 0, f.fs.finishLocked(op, constraint("faultvfs.write_at", "offset cannot be negative"))
	}
	if offset > int64(^uint(0)>>1)-int64(len(buffer)) {
		return 0, f.fs.finishLocked(op, dberrors.New(dberrors.CodeResourceLimit, "faultvfs.write_at", "write size overflows address space"))
	}
	fault, hasFault := f.fs.faultLocked(Write)
	if hasFault && fault.Action == Fail {
		return 0, f.fs.finishLocked(op, injected(Write))
	}
	count := len(buffer)
	err := error(nil)
	if hasFault && fault.Action == Short {
		count = min(fault.Bytes, len(buffer))
		err = ioError(io.ErrShortWrite, "faultvfs.write_at", f.path)
	}
	data := slices.Clone(buffer[:count])
	writeBytes(&f.state.visible, offset, data)
	durableBytes := count
	if hasFault && fault.Action == Tear {
		durableBytes = min(fault.Bytes, count)
	}
	f.state.pending = append(f.state.pending, mutation{kind: writeMutation, offset: offset, data: data, durableBytes: durableBytes})
	op.Completed = count
	return count, f.fs.finishLocked(op, err)
}

func writeBytes(target *[]byte, offset int64, data []byte) {
	if len(data) == 0 {
		return
	}
	end := int(offset) + len(data)
	if end > len(*target) {
		*target = append(*target, make([]byte, end-len(*target))...)
	}
	copy((*target)[int(offset):], data)
}

func (f *file) Sync() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Sync, Path: f.path}
	if err := f.usableLocked("faultvfs.sync"); err != nil {
		return f.fs.finishLocked(op, err)
	}
	if fault, ok := f.fs.faultLocked(Sync); ok && fault.Action == Fail {
		return f.fs.finishLocked(op, injected(Sync))
	}
	behavior := f.fs.syncBehaviorLocked()
	indexes, err := selectedIndexes(behavior, len(f.state.pending))
	if err != nil {
		return f.fs.finishLocked(op, err)
	}
	selected := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		applyMutation(&f.state.durable, f.state.pending[index])
		selected[index] = struct{}{}
	}
	remaining := f.state.pending[:0]
	for index, pending := range f.state.pending {
		if _, persisted := selected[index]; !persisted {
			remaining = append(remaining, pending)
		}
	}
	f.state.pending = remaining
	return f.fs.finishLocked(op, nil)
}

func selectedIndexes(behavior SyncBehavior, count int) ([]int, error) {
	switch behavior.Mode {
	case SyncAll:
		indexes := make([]int, count)
		for index := range indexes {
			indexes[index] = index
		}
		return indexes, nil
	case SyncNone:
		return nil, nil
	case SyncSelected:
		indexes := make([]int, 0, len(behavior.MutationOrder))
		seen := make(map[int]struct{}, len(behavior.MutationOrder))
		for _, oneBased := range behavior.MutationOrder {
			index := oneBased - 1
			if index < 0 || index >= count {
				return nil, constraint("faultvfs.sync", "selected mutation is outside the pending list")
			}
			if _, duplicate := seen[index]; duplicate {
				return nil, constraint("faultvfs.sync", "selected mutation is duplicated")
			}
			seen[index] = struct{}{}
			indexes = append(indexes, index)
		}
		return indexes, nil
	default:
		panic("validated sync mode")
	}
}

func applyMutation(target *[]byte, pending mutation) {
	if pending.kind == truncateMutation {
		if pending.size <= int64(len(*target)) {
			*target = (*target)[:pending.size]
		} else {
			*target = append(*target, make([]byte, int(pending.size)-len(*target))...)
		}
		return
	}
	writeBytes(target, pending.offset, pending.data[:pending.durableBytes])
}

func (f *file) Truncate(size int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Truncate, Path: f.path, Offset: size}
	if err := f.writableLocked("faultvfs.truncate"); err != nil {
		return f.fs.finishLocked(op, err)
	}
	if size < 0 {
		return f.fs.finishLocked(op, constraint("faultvfs.truncate", "size cannot be negative"))
	}
	if size > int64(^uint(0)>>1) {
		return f.fs.finishLocked(op, dberrors.New(dberrors.CodeResourceLimit, "faultvfs.truncate", "size overflows address space"))
	}
	if fault, ok := f.fs.faultLocked(Truncate); ok && fault.Action == Fail {
		return f.fs.finishLocked(op, injected(Truncate))
	}
	if size <= int64(len(f.state.visible)) {
		f.state.visible = f.state.visible[:size]
	} else {
		f.state.visible = append(f.state.visible, make([]byte, int(size)-len(f.state.visible))...)
	}
	f.state.pending = append(f.state.pending, mutation{kind: truncateMutation, size: size})
	return f.fs.finishLocked(op, nil)
}

func (f *file) Size() (int64, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Size, Path: f.path}
	if err := f.usableLocked("faultvfs.size"); err != nil {
		return 0, f.fs.finishLocked(op, err)
	}
	size := int64(len(f.state.visible))
	op.Completed = len(f.state.visible)
	return size, f.fs.finishLocked(op, nil)
}

func (f *file) Lock(mode vfs.LockMode) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Lock, Path: f.path}
	if err := f.usableLocked("faultvfs.lock"); err != nil {
		return f.fs.finishLocked(op, err)
	}
	if mode != vfs.LockShared && mode != vfs.LockExclusive {
		return f.fs.finishLocked(op, constraint("faultvfs.lock", fmt.Sprintf("invalid lock mode %d", mode)))
	}
	if mode == vfs.LockShared {
		if f.state.exclusive != 0 && f.state.exclusive != f.id {
			return f.fs.finishLocked(op, conflict("faultvfs.lock", f.path))
		}
		f.state.shared[f.id] = struct{}{}
	} else {
		_, ownsShared := f.state.shared[f.id]
		otherShared := len(f.state.shared)
		if ownsShared {
			otherShared--
		}
		if (f.state.exclusive != 0 && f.state.exclusive != f.id) || otherShared != 0 {
			return f.fs.finishLocked(op, conflict("faultvfs.lock", f.path))
		}
		delete(f.state.shared, f.id)
		f.state.exclusive = f.id
	}
	return f.fs.finishLocked(op, nil)
}

func (f *file) Unlock() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Unlock, Path: f.path}
	if err := f.usableLocked("faultvfs.unlock"); err != nil {
		return f.fs.finishLocked(op, err)
	}
	delete(f.state.shared, f.id)
	if f.state.exclusive == f.id {
		f.state.exclusive = 0
	}
	return f.fs.finishLocked(op, nil)
}

func (f *file) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	op := Operation{Kind: Close, Path: f.path}
	if f.closed {
		return f.fs.finishLocked(op, constraint("faultvfs.close", "file is already closed"))
	}
	if f.epoch != f.fs.epoch {
		return f.fs.finishLocked(op, ioError(ErrCrash, "faultvfs.close", f.path))
	}
	delete(f.state.shared, f.id)
	if f.state.exclusive == f.id {
		f.state.exclusive = 0
	}
	f.closed = true
	return f.fs.finishLocked(op, nil)
}

func (f *file) Path() string { return f.path }

func (f *file) usableLocked(operation string) error {
	if f.closed {
		return constraint(operation, "file is closed")
	}
	if f.epoch != f.fs.epoch {
		return ioError(ErrCrash, operation, f.path)
	}
	return nil
}

func (f *file) writableLocked(operation string) error {
	if err := f.usableLocked(operation); err != nil {
		return err
	}
	if f.readOnly {
		return constraint(operation, "file is read-only")
	}
	return nil
}

func injected(kind Kind) error {
	return ioError(fmt.Errorf("injected %s failure", kind), "faultvfs."+string(kind), "")
}

func ioError(cause error, operation, path string) error {
	message := "injected I/O failure"
	if path != "" {
		message = fmt.Sprintf("file %q", path)
	}
	return dberrors.Wrap(cause, dberrors.CodeIO, operation, message)
}

func constraint(operation, message string) error {
	return dberrors.New(dberrors.CodeConstraint, operation, message)
}

func conflict(operation, path string) error {
	return dberrors.New(dberrors.CodeConflict, operation, fmt.Sprintf("file %q is already locked", path))
}

var _ vfs.VFS = (*FS)(nil)
var _ vfs.File = (*file)(nil)
