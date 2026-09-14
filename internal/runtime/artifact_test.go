package runtime

import (
	"fmt"
	"io"
	"sync"
	"testing"
)

func TestArtifactStoresSharingDirectoryAllocateUniqueIDs(t *testing.T) {
	dir := t.TempDir()
	stores := []*ArtifactStore{NewArtifactStore(dir), NewArtifactStore(dir)}
	const perStore = 50

	ids := make(chan string, len(stores)*perStore)
	errCh := make(chan error, len(stores))
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(worker int, s *ArtifactStore) {
			defer wg.Done()
			for n := 0; n < perStore; n++ {
				id, f, err := s.Create(fmt.Sprintf("worker-%d", worker))
				if err != nil {
					errCh <- err
					return
				}
				if _, err := io.WriteString(f, id); err != nil {
					_ = f.Close()
					errCh <- err
					return
				}
				if err := f.Close(); err != nil {
					errCh <- err
					return
				}
				ids <- id
			}
		}(i, store)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	close(ids)

	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate artifact id %s", id)
		}
		seen[id] = true
	}
	if len(seen) != len(stores)*perStore {
		t.Fatalf("allocated %d unique ids, want %d", len(seen), len(stores)*perStore)
	}
}
