package btree_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"testing"

	"yodadb/dberrors"
	"yodadb/storage/btree"
	"yodadb/storage/page"
	"yodadb/types"
)

func TestBTreeModelFast(t *testing.T) {
	seeds := []int64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233}
	runBTreeModelSeeds(t, seeds, 1500, 256, 512)
}

func TestBTreeModelExtended(t *testing.T) {
	if os.Getenv("YODADB_EXTENDED_MODEL") != "1" {
		t.Skip("set YODADB_EXTENDED_MODEL=1 to run 3.2 million randomized operations")
	}
	seeds := make([]int64, 32)
	for index := range seeds {
		seeds[index] = 1009 + int64(index)*7919
	}
	runBTreeModelSeeds(t, seeds, 100_000, 4096, 512)
}

func runBTreeModelSeeds(t *testing.T, seeds []int64, operations, keyspace, maximumValue int) {
	t.Helper()
	for _, seed := range seeds {
		seed := seed
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			runBTreeModel(t, seed, operations, keyspace, maximumValue)
		})
	}
}

func runBTreeModel(t *testing.T, seed int64, operations, keyspace, maximumValue int) {
	t.Helper()
	ctx := context.Background()
	random := rand.New(rand.NewSource(seed))
	store := newModelPageStore()
	tree, err := btree.Open(store, 0)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([][]byte, keyspace)
	for number := range keyspace {
		keys[number], err = types.EncodeKey(types.Row{types.Int64(int64(number))}, types.KeyLimits{})
		if err != nil {
			t.Fatal(err)
		}
	}
	model := make(map[int][]byte)
	snapshots := make([]modelSnapshot, 0, 8)
	snapshotEvery := max(1, operations/8)

	for operation := range operations {
		number := random.Intn(keyspace)
		choice := random.Intn(100)
		switch {
		case choice < 25:
			value := randomValue(random, maximumValue)
			beforeRoot, beforePages := tree.Root(), store.pageCount()
			beforeValue, existed := model[number]
			root, insertErr := tree.Insert(ctx, store, uint64(operation+1), keys[number], value)
			if existed {
				if !errors.Is(insertErr, dberrors.ErrConstraint) || root != 0 {
					t.Fatalf("operation %d duplicate Insert(%d) = root %d, error %v", operation, number, root, insertErr)
				}
				if store.pageCount() != beforePages {
					t.Fatalf("operation %d duplicate Insert(%d) appended pages", operation, number)
				}
				assertModelGet(t, tree, keys[number], beforeValue, true, seed, operation)
				break
			}
			if insertErr != nil {
				t.Fatalf("operation %d Insert(%d): %v", operation, number, insertErr)
			}
			oldTree := tree
			tree, err = btree.Open(store, root)
			if err != nil {
				t.Fatal(err)
			}
			model[number] = bytes.Clone(value)
			assertModelGet(t, oldTree, keys[number], nil, false, seed, operation)
			if oldTree.Root() != beforeRoot {
				t.Fatalf("operation %d old tree root changed", operation)
			}

		case choice < 40:
			beforeRoot, beforePages := tree.Root(), store.pageCount()
			beforeValue, existed := model[number]
			root, deleted, deleteErr := tree.Delete(ctx, store, uint64(operation+1), keys[number])
			if deleteErr != nil {
				t.Fatalf("operation %d Delete(%d): %v", operation, number, deleteErr)
			}
			if deleted != existed {
				t.Fatalf("operation %d Delete(%d) deleted=%t, want %t", operation, number, deleted, existed)
			}
			if !existed {
				if root != beforeRoot || store.pageCount() != beforePages {
					t.Fatalf("operation %d missing Delete(%d) changed root or pages", operation, number)
				}
				break
			}
			oldTree := tree
			tree, err = btree.Open(store, root)
			if err != nil {
				t.Fatal(err)
			}
			delete(model, number)
			assertModelGet(t, oldTree, keys[number], beforeValue, true, seed, operation)
			if oldTree.Root() != beforeRoot {
				t.Fatalf("operation %d old tree root changed", operation)
			}

		case choice < 97:
			value, found := model[number]
			assertModelGet(t, tree, keys[number], value, found, seed, operation)

		case choice < 99:
			assertModelCursor(t, tree, keys, model, number, keyspace, seed, operation)

		default:
			width := random.Intn(129)
			end := min(keyspace, number+width)
			assertModelRange(t, tree, keys, model, number, end, seed, operation)
		}

		if (operation+1)%1024 == 0 {
			assertModelCursor(t, tree, keys, model, 0, keyspace, seed, operation)
		}
		if (operation+1)%snapshotEvery == 0 && len(snapshots) < cap(snapshots) {
			snapshots = append(snapshots, modelSnapshot{root: tree.Root(), values: cloneModel(model)})
			roots := make([]page.ID, 0, len(snapshots)+1)
			roots = append(roots, tree.Root())
			for _, snapshot := range snapshots {
				roots = append(roots, snapshot.root)
			}
			if err := store.retainReachable(roots); err != nil {
				t.Fatalf("operation %d prune: %v", operation, err)
			}
		}
	}

	assertModelCursor(t, tree, keys, model, 0, keyspace, seed, operations)
	for index, snapshot := range snapshots {
		snapshotTree, err := btree.Open(store, snapshot.root)
		if err != nil {
			t.Fatal(err)
		}
		assertModelCursor(t, snapshotTree, keys, snapshot.values, 0, keyspace, seed, operations+index+1)
	}
}

func randomValue(random *rand.Rand, maximum int) []byte {
	value := make([]byte, random.Intn(maximum+1))
	_, _ = random.Read(value)
	return value
}

func assertModelGet(t *testing.T, tree *btree.Tree, key, want []byte, wantFound bool, seed int64, operation int) {
	t.Helper()
	got, found, err := tree.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("seed %d operation %d Get: %v", seed, operation, err)
	}
	if found != wantFound || !bytes.Equal(got, want) {
		t.Fatalf("seed %d operation %d Get = %x, %t; want %x, %t", seed, operation, got, found, want, wantFound)
	}
}

func assertModelCursor(t *testing.T, tree *btree.Tree, keys [][]byte, model map[int][]byte, start, end int, seed int64, operation int) {
	t.Helper()
	cursor, err := tree.Seek(context.Background(), keys[start])
	if err != nil {
		t.Fatalf("seed %d operation %d Seek: %v", seed, operation, err)
	}
	assertCursorMatchesModel(t, cursor, keys, model, start, end, seed, operation)
}

func assertModelRange(t *testing.T, tree *btree.Tree, keys [][]byte, model map[int][]byte, start, end int, seed int64, operation int) {
	t.Helper()
	var endKey []byte
	if end < len(keys) {
		endKey = keys[end]
	}
	cursor, err := tree.Range(context.Background(), keys[start], endKey)
	if err != nil {
		t.Fatalf("seed %d operation %d Range: %v", seed, operation, err)
	}
	assertCursorMatchesModel(t, cursor, keys, model, start, end, seed, operation)
}

func assertCursorMatchesModel(t *testing.T, cursor *btree.Cursor, keys [][]byte, model map[int][]byte, start, end int, seed int64, operation int) {
	t.Helper()
	for number := start; number < end; number++ {
		want, live := model[number]
		if !live {
			continue
		}
		key, value, err := cursor.Next(context.Background())
		if err != nil {
			t.Fatalf("seed %d operation %d cursor ended before key %d: %v", seed, operation, number, err)
		}
		if !bytes.Equal(key, keys[number]) || !bytes.Equal(value, want) {
			t.Fatalf("seed %d operation %d cursor = %x/%x; want key %d %x/%x", seed, operation, key, value, number, keys[number], want)
		}
	}
	if _, _, err := cursor.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("seed %d operation %d cursor has unexpected trailing entry: %v", seed, operation, err)
	}
}

type modelSnapshot struct {
	root   page.ID
	values map[int][]byte
}

func cloneModel(model map[int][]byte) map[int][]byte {
	cloned := make(map[int][]byte, len(model))
	for key, value := range model {
		cloned[key] = bytes.Clone(value)
	}
	return cloned
}

type modelPageStore struct {
	next  page.ID
	pages map[page.ID]page.Page
}

func newModelPageStore() *modelPageStore {
	return &modelPageStore{next: 2, pages: make(map[page.ID]page.Page)}
}

func (s *modelPageStore) Append(pageType page.Type, generation uint64, payload []byte) (page.ID, error) {
	id := s.next
	s.next++
	s.pages[id] = page.Page{
		Header:  page.Header{Type: pageType, PageID: id, Generation: generation},
		Payload: bytes.Clone(payload),
	}
	return id, nil
}

func (s *modelPageStore) ReadPage(id page.ID) (page.Page, error) {
	stored, ok := s.pages[id]
	if !ok {
		return page.Page{}, fmt.Errorf("model store missing page %d", id)
	}
	return stored, nil
}

func (s *modelPageStore) pageCount() int { return len(s.pages) }

func (s *modelPageStore) retainReachable(roots []page.ID) error {
	reachable := make(map[page.ID]struct{})
	pending := append([]page.ID(nil), roots...)
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if id == 0 {
			continue
		}
		if _, seen := reachable[id]; seen {
			continue
		}
		stored, ok := s.pages[id]
		if !ok {
			return fmt.Errorf("reachable page %d is absent", id)
		}
		reachable[id] = struct{}{}
		switch stored.Header.Type {
		case page.TypeBTreeLeaf:
			if _, err := page.DecodeLeaf(stored.Payload); err != nil {
				return err
			}
		case page.TypeBTreeInternal:
			internal, err := page.DecodeInternal(stored.Payload)
			if err != nil {
				return err
			}
			pending = append(pending, internal.FirstChild)
			for _, entry := range internal.Entries {
				pending = append(pending, entry.RightChild)
			}
		default:
			return fmt.Errorf("reachable page %d has type %s", id, stored.Header.Type)
		}
	}
	for id := range s.pages {
		if _, keep := reachable[id]; !keep {
			delete(s.pages, id)
		}
	}
	return nil
}

var _ btree.PageSource = (*modelPageStore)(nil)
var _ btree.PageAppender = (*modelPageStore)(nil)
