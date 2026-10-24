package manifest

import (
	"fmt"

	"yodadb/dberrors"
	"yodadb/storage/page"
	"yodadb/storage/vfs"
)

// completePagesFile hides an incomplete crash tail from the fixed-page pager.
// Reads and later appends still use the underlying file; Size exposes only the
// prefix consisting of complete pages.
type completePagesFile struct {
	vfs.File
}

func (f *completePagesFile) Size() (int64, error) {
	size, err := f.File.Size()
	if err != nil {
		return 0, err
	}
	return size - size%page.Size, nil
}

func validateCandidate(file vfs.File, pageCount uint64, candidate Candidate) error {
	meta := candidate.Meta
	if meta.LatestTxnID == 0 {
		if meta.LatestManifestPageID != 0 || meta.OldestRetainedTxnID != 0 {
			return candidateCorruption(candidate, "empty state contains transaction metadata")
		}
		return nil
	}
	if meta.LatestManifestPageID < 2 {
		return candidateCorruption(candidate, "non-empty state does not reference a manifest")
	}
	stored, err := readValidatedPage(file, pageCount, meta.LatestManifestPageID)
	if err != nil {
		return candidateCorruption(candidate, fmt.Sprintf("read manifest page %d: %v", meta.LatestManifestPageID, err))
	}
	if stored.Header.Type != page.TypeManifest {
		return candidateCorruption(candidate, fmt.Sprintf("page %d is %s, expected manifest", stored.Header.PageID, stored.Header.Type))
	}
	if stored.Header.Generation != meta.LatestTxnID {
		return candidateCorruption(candidate, "manifest page generation disagrees with transaction ID")
	}
	record, err := DecodeManifest(stored.Payload)
	if err != nil {
		return candidateCorruption(candidate, fmt.Sprintf("decode manifest page %d: %v", stored.Header.PageID, err))
	}
	if record.TxnID != meta.LatestTxnID {
		return candidateCorruption(candidate, "manifest transaction disagrees with metapage")
	}
	if record.OldestRetainedTxnID != meta.OldestRetainedTxnID {
		return candidateCorruption(candidate, "manifest retention boundary disagrees with metapage")
	}

	roots := make(map[page.ID]string, 1+len(record.TableRoots)+len(record.IndexRoots))
	addRoot := func(root page.ID, description string) {
		if root != 0 {
			if _, exists := roots[root]; !exists {
				roots[root] = description
			}
		}
	}
	addRoot(record.CatalogRoot, "catalog")
	addRoot(record.ChangeRoot, "change")
	for id, root := range record.TableRoots {
		addRoot(root, fmt.Sprintf("table %d", id))
	}
	for id, root := range record.IndexRoots {
		addRoot(root, fmt.Sprintf("index %d", id))
	}
	for root, description := range roots {
		if err := validateTreeRoot(file, pageCount, record.TxnID, root); err != nil {
			return candidateCorruption(candidate, fmt.Sprintf("%s root %d: %v", description, root, err))
		}
	}
	return nil
}

func validateTreeRoot(file vfs.File, pageCount, txnID uint64, root page.ID) error {
	stored, err := readValidatedPage(file, pageCount, root)
	if err != nil {
		return err
	}
	if stored.Header.Generation == 0 || stored.Header.Generation > txnID {
		return fmt.Errorf("page generation %d is outside transaction %d", stored.Header.Generation, txnID)
	}
	switch stored.Header.Type {
	case page.TypeBTreeLeaf:
		leaf, err := page.DecodeLeaf(stored.Payload)
		if err != nil {
			return err
		}
		if leaf.RightSibling != 0 && (leaf.RightSibling < 2 || uint64(leaf.RightSibling) >= pageCount) {
			return fmt.Errorf("right sibling %d is outside the file", leaf.RightSibling)
		}
	case page.TypeBTreeInternal:
		internal, err := page.DecodeInternal(stored.Payload)
		if err != nil {
			return err
		}
		if uint64(internal.FirstChild) >= pageCount {
			return fmt.Errorf("first child %d is outside the file", internal.FirstChild)
		}
		for _, entry := range internal.Entries {
			if uint64(entry.RightChild) >= pageCount {
				return fmt.Errorf("right child %d is outside the file", entry.RightChild)
			}
		}
	default:
		return fmt.Errorf("page type is %s, expected B+ tree root", stored.Header.Type)
	}
	return nil
}

func readValidatedPage(file vfs.File, pageCount uint64, id page.ID) (page.Page, error) {
	if id < 2 || uint64(id) >= pageCount {
		return page.Page{}, fmt.Errorf("page %d is outside file page count %d", id, pageCount)
	}
	buffer := make([]byte, page.Size)
	if err := readFullAt(file, buffer, int64(id)*page.Size); err != nil {
		return page.Page{}, err
	}
	stored, err := page.Decode(buffer)
	if err != nil {
		return page.Page{}, err
	}
	if stored.Header.PageID != id {
		return page.Page{}, fmt.Errorf("page at position %d declares ID %d", id, stored.Header.PageID)
	}
	return stored, nil
}

func candidateCorruption(candidate Candidate, invariant string) error {
	return dberrors.New(dberrors.CodeCorruption, "database.validate_candidate", fmt.Sprintf("metapage %d generation %d: %s", candidate.Slot, candidate.Generation, invariant))
}
