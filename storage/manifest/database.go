package manifest

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sync"

	"yodadb/dberrors"
	"yodadb/storage/page"
	"yodadb/storage/pager"
	"yodadb/storage/vfs"
)

// Options controls low-level database opening.
type Options struct {
	CachePages int
	ReadOnly   bool
	Durability Durability
}

// Metadata is the selected metapage state and its physical slot.
type Metadata struct {
	Meta
	ActiveSlot page.ID
	Generation uint64
}

// Database owns the page file, selected committed generation, and publication
// state.
type Database struct {
	mu          sync.Mutex
	path        string
	file        vfs.File
	pager       *pager.Pager
	metadata    Metadata
	readOnly    bool
	durability  Durability
	closed      bool
	writeFailed bool
}

// Create creates and opens a new empty database with the OS VFS.
func Create(ctx context.Context, path string, options Options) (*Database, error) {
	return CreateWithVFS(ctx, vfs.OS{}, path, options)
}

// CreateWithVFS creates and opens a new empty database through filesystem.
func CreateWithVFS(ctx context.Context, filesystem vfs.VFS, path string, options Options) (*Database, error) {
	if err := contextFailure(ctx, "database.create"); err != nil {
		return nil, err
	}
	if err := validateDurability(options.Durability, "database.create"); err != nil {
		return nil, err
	}
	if options.ReadOnly {
		return nil, dberrors.New(dberrors.CodeConstraint, "database.create", "cannot create a read-only database")
	}
	file, err := filesystem.Open(path, vfs.OpenOptions{Create: true, Exclusive: true})
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	if err := file.Lock(vfs.LockExclusive); err != nil {
		return nil, err
	}
	var fileID FileID
	if _, err := rand.Read(fileID[:]); err != nil {
		return nil, dberrors.Wrap(err, dberrors.CodeIO, "database.create", "generate file identity")
	}
	meta := Meta{FileID: fileID, PageSize: page.Size}
	first, err := EncodeMetapage(0, 1, meta)
	if err != nil {
		return nil, err
	}
	second, err := EncodeMetapage(1, 0, meta)
	if err != nil {
		return nil, err
	}
	if err := writeFullAt(file, first, 0); err != nil {
		return nil, dberrors.Wrap(err, dberrors.CodeIO, "database.create", "write metapage 0")
	}
	if err := writeFullAt(file, second, page.Size); err != nil {
		return nil, dberrors.Wrap(err, dberrors.CodeIO, "database.create", "write metapage 1")
	}
	if options.Durability == DurabilityFull {
		if err := file.Sync(); err != nil {
			return nil, err
		}
	}
	pageManager, err := pager.Open(file, options.CachePages)
	if err != nil {
		return nil, err
	}
	keep = true
	return &Database{
		path:       path,
		file:       file,
		pager:      pageManager,
		metadata:   Metadata{Meta: meta, ActiveSlot: 0, Generation: 1},
		durability: options.Durability,
	}, nil
}

// Open opens an existing database with the OS VFS.
func Open(ctx context.Context, path string, options Options) (*Database, error) {
	return OpenWithVFS(ctx, vfs.OS{}, path, options)
}

// OpenWithVFS opens an existing database and selects its newest valid metapage.
func OpenWithVFS(ctx context.Context, filesystem vfs.VFS, path string, options Options) (*Database, error) {
	if err := contextFailure(ctx, "database.open"); err != nil {
		return nil, err
	}
	if err := validateDurability(options.Durability, "database.open"); err != nil {
		return nil, err
	}
	file, err := filesystem.Open(path, vfs.OpenOptions{ReadOnly: options.ReadOnly})
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	lockMode := vfs.LockExclusive
	if options.ReadOnly {
		lockMode = vfs.LockShared
	}
	if err := file.Lock(lockMode); err != nil {
		return nil, err
	}
	size, err := file.Size()
	if err != nil {
		return nil, err
	}
	if size%page.Size != 0 {
		file = &completePagesFile{File: file}
		size -= size % page.Size
	}
	if size < 2*page.Size {
		return nil, dberrors.New(dberrors.CodeCorruption, "database.open", fmt.Sprintf("file has %d bytes, fewer than two metapages", size))
	}

	decodedCandidates := make([]Candidate, 0, 2)
	var candidateErrors []error
	for slot := page.ID(0); slot < 2; slot++ {
		buffer := make([]byte, page.Size)
		if err := readFullAt(file, buffer, int64(slot)*page.Size); err != nil {
			if dberrors.CodeOf(err) == dberrors.CodeIO {
				return nil, dberrors.Wrap(err, dberrors.CodeIO, "database.open", fmt.Sprintf("read metapage %d", slot))
			}
			candidateErrors = append(candidateErrors, fmt.Errorf("metapage %d: %w", slot, err))
			continue
		}
		candidate, err := DecodeMetapage(buffer, slot)
		if err != nil {
			candidateErrors = append(candidateErrors, err)
			continue
		}
		decodedCandidates = append(decodedCandidates, candidate)
	}
	if len(decodedCandidates) == 0 {
		return nil, dberrors.Wrap(errors.Join(candidateErrors...), dberrors.CodeCorruption, "database.open", "neither metapage is valid")
	}
	pageCount := uint64(size / page.Size)
	candidates := make([]Candidate, 0, len(decodedCandidates))
	for _, candidate := range decodedCandidates {
		if err := validateCandidate(file, pageCount, candidate); err != nil {
			if dberrors.CodeOf(err) == dberrors.CodeIO {
				return nil, err
			}
			candidateErrors = append(candidateErrors, fmt.Errorf("metapage %d generation %d: %w", candidate.Slot, candidate.Generation, err))
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, dberrors.Wrap(errors.Join(candidateErrors...), dberrors.CodeCorruption, "database.open", "no metapage references a valid committed state")
	}
	if len(candidates) == 2 {
		if candidates[0].Meta.FileID != candidates[1].Meta.FileID || candidates[0].Meta.PageSize != candidates[1].Meta.PageSize {
			return nil, dberrors.New(dberrors.CodeCorruption, "database.open", "valid metapages disagree on file identity or page size")
		}
		if candidates[0].Generation == candidates[1].Generation {
			return nil, dberrors.New(dberrors.CodeCorruption, "database.open", "valid metapages have the same generation")
		}
	}
	selected := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.Generation > selected.Generation {
			selected = candidate
		}
	}
	pageManager, err := pager.Open(file, options.CachePages)
	if err != nil {
		return nil, err
	}
	keep = true
	return &Database{
		path:       path,
		file:       file,
		pager:      pageManager,
		metadata:   Metadata{Meta: selected.Meta, ActiveSlot: selected.Slot, Generation: selected.Generation},
		readOnly:   options.ReadOnly,
		durability: options.Durability,
	}, nil
}

// Metadata returns the selected immutable metapage information.
func (d *Database) Metadata() Metadata {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.metadata
}

// Durability returns the commit durability selected when the handle opened.
func (d *Database) Durability() Durability {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.durability
}

// PageCount returns the current number of complete pages.
func (d *Database) PageCount() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0
	}
	return d.pager.PageCount()
}

// ReadPage reads and releases one validated page.
func (d *Database) ReadPage(id page.ID) (page.Page, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return page.Page{}, dberrors.New(dberrors.CodeTransactionClosed, "database.read_page", "database is closed")
	}
	handle, err := d.pager.Read(id)
	if err != nil {
		return page.Page{}, err
	}
	defer handle.Release()
	return handle.Page(), nil
}

// Append appends an immutable non-metapage. It does not publish a transaction.
func (d *Database) Append(pageType page.Type, generation uint64, payload []byte) (page.ID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, dberrors.New(dberrors.CodeTransactionClosed, "database.append", "database is closed")
	}
	if d.readOnly {
		return 0, dberrors.New(dberrors.CodeConstraint, "database.append", "database is read-only")
	}
	if d.writeFailed {
		return 0, dberrors.New(dberrors.CodeTransactionClosed, "database.append", "database requires reopen after a failed write")
	}
	id, err := d.pager.Append(pageType, generation, payload)
	if err != nil && dberrors.CodeOf(err) == dberrors.CodeIO {
		d.writeFailed = true
	}
	return id, err
}

// Sync synchronizes appended pages without publishing a transaction.
func (d *Database) Sync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return dberrors.New(dberrors.CodeTransactionClosed, "database.sync", "database is closed")
	}
	if d.readOnly {
		return nil
	}
	if d.writeFailed {
		return dberrors.New(dberrors.CodeTransactionClosed, "database.sync", "database requires reopen after a failed write")
	}
	if err := d.pager.Sync(); err != nil {
		d.writeFailed = true
		return err
	}
	return nil
}

// LatestManifest reads the manifest selected by the current metapage. An empty
// database returns found=false.
func (d *Database) LatestManifest() (Manifest, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return Manifest{}, false, dberrors.New(dberrors.CodeTransactionClosed, "database.latest_manifest", "database is closed")
	}
	if d.metadata.LatestManifestPageID == 0 {
		return Manifest{}, false, nil
	}
	handle, err := d.pager.Read(d.metadata.LatestManifestPageID)
	if err != nil {
		return Manifest{}, false, err
	}
	defer handle.Release()
	stored := handle.Page()
	if stored.Header.Type != page.TypeManifest {
		return Manifest{}, false, dberrors.New(dberrors.CodeCorruption, "database.latest_manifest", fmt.Sprintf("page %d has type %s", stored.Header.PageID, stored.Header.Type))
	}
	record, err := DecodeManifest(stored.Payload)
	if err != nil {
		return Manifest{}, false, err
	}
	if record.TxnID != d.metadata.LatestTxnID {
		return Manifest{}, false, dberrors.New(dberrors.CodeCorruption, "database.latest_manifest", "manifest transaction disagrees with metapage")
	}
	return record, true, nil
}

// Publish appends and synchronizes a manifest, then publishes it through the
// older metapage slot. A publication I/O failure rejects later writes until the
// database is reopened and recovery selects a valid generation.
func (d *Database) Publish(ctx context.Context, record Manifest) (page.ID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return 0, dberrors.New(dberrors.CodeTransactionClosed, "database.publish", "database is closed")
	}
	if d.readOnly {
		return 0, dberrors.New(dberrors.CodeConstraint, "database.publish", "database is read-only")
	}
	if d.writeFailed {
		return 0, dberrors.New(dberrors.CodeTransactionClosed, "database.publish", "database requires reopen after a failed write")
	}
	if err := contextFailure(ctx, "database.publish"); err != nil {
		return 0, err
	}
	if record.TxnID != d.metadata.LatestTxnID+1 || record.ParentTxnID != d.metadata.LatestTxnID {
		return 0, dberrors.New(dberrors.CodeConflict, "database.publish", "manifest does not extend the selected transaction")
	}
	if d.metadata.OldestRetainedTxnID != 0 && record.OldestRetainedTxnID < d.metadata.OldestRetainedTxnID {
		return 0, dberrors.New(dberrors.CodeHistoryNotRetained, "database.publish", "retained-history boundary cannot move backward")
	}
	if d.metadata.Generation == ^uint64(0) {
		return 0, dberrors.New(dberrors.CodeResourceLimit, "database.publish", "metapage generation space is exhausted")
	}
	payload, err := EncodeManifest(record)
	if err != nil {
		return 0, err
	}
	manifestID, err := d.pager.Append(page.TypeManifest, record.TxnID, payload)
	if err != nil {
		if dberrors.CodeOf(err) == dberrors.CodeIO {
			d.writeFailed = true
		}
		return 0, err
	}
	if d.durability == DurabilityFull {
		if err := d.pager.Sync(); err != nil {
			d.writeFailed = true
			return 0, err
		}
	}
	if err := contextFailure(ctx, "database.publish"); err != nil {
		return 0, err
	}
	target := page.ID(1)
	if d.metadata.ActiveSlot == 1 {
		target = 0
	}
	meta := d.metadata.Meta
	meta.LatestTxnID = record.TxnID
	meta.LatestManifestPageID = manifestID
	meta.OldestRetainedTxnID = record.OldestRetainedTxnID
	generation := d.metadata.Generation + 1
	encoded, err := EncodeMetapage(target, generation, meta)
	if err != nil {
		return 0, err
	}
	if err := writeFullAt(d.file, encoded, int64(target)*page.Size); err != nil {
		d.writeFailed = true
		return 0, dberrors.Wrap(err, dberrors.CodeIO, "database.publish", "write metapage")
	}
	if d.durability == DurabilityFull {
		if err := d.file.Sync(); err != nil {
			d.writeFailed = true
			return 0, err
		}
	}
	d.metadata = Metadata{Meta: meta, ActiveSlot: target, Generation: generation}
	return manifestID, nil
}

// Close releases the pager, file, and advisory lock.
func (d *Database) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	if err := d.pager.Close(); err != nil {
		return err
	}
	d.closed = true
	return nil
}

// Path returns the path supplied when the database was opened.
func (d *Database) Path() string { return d.path }

func contextFailure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return dberrors.Wrap(err, dberrors.CodeCancelled, operation, "context cancelled")
	}
	return nil
}

func readFullAt(file vfs.File, buffer []byte, offset int64) error {
	read := 0
	for read < len(buffer) {
		count, err := file.ReadAt(buffer[read:], offset+int64(read))
		read += count
		if err != nil {
			return err
		}
		if read == len(buffer) {
			return nil
		}
		if count == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}

func writeFullAt(file vfs.File, buffer []byte, offset int64) error {
	written := 0
	for written < len(buffer) {
		count, err := file.WriteAt(buffer[written:], offset+int64(written))
		written += count
		if err != nil {
			return err
		}
		if written == len(buffer) {
			return nil
		}
		if count == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
