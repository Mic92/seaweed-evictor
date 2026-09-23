package evictor_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Mic92/seaweed-evictor/internal/evictor"
	"github.com/Mic92/seaweed-evictor/internal/fluentin"
)

type fakeFiler struct {
	mu       sync.Mutex
	initial  []evictor.Entry
	changes  chan evictor.Change
	uncached []string
	failNext map[string]bool
	busy     map[string]bool // entries that turned dirty behind our back
	attempts map[string]int
}

func newFake(initial ...evictor.Entry) *fakeFiler {
	return &fakeFiler{
		initial:  initial,
		changes:  make(chan evictor.Change, 64),
		failNext: map[string]bool{},
		busy:     map[string]bool{},
		attempts: map[string]int{},
	}
}

func (f *fakeFiler) Walk(_ context.Context, _ string, fn func(evictor.Entry)) (int64, error) {
	for _, e := range f.initial {
		fn(e)
	}

	return 42, nil
}

func (f *fakeFiler) Subscribe(ctx context.Context, _ string, since int64, fn func(evictor.Change)) error {
	if since != 42 {
		return fmt.Errorf("subscribed since %d, want walk timestamp 42", since)
	}
	for {
		select {
		case c := <-f.changes:
			fn(c)
		case <-ctx.Done():
			return nil
		}
	}
}

func (f *fakeFiler) Uncache(_ context.Context, path string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[path]++
	if f.failNext[path] {
		delete(f.failNext, path)

		return false, errors.New("filer unavailable")
	}
	if f.busy[path] {
		return false, nil
	}
	f.uncached = append(f.uncached, path)

	return true, nil
}

func (f *fakeFiler) Attempts(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.attempts[path]
}

func (f *fakeFiler) Uncached() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.uncached...)
}

// Pending counts injected failures not yet consumed.
func (f *fakeFiler) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.failNext)
}

func synced(path string) evictor.Entry {
	return evictor.Entry{Path: path, Size: 10, State: evictor.Synced}
}

func run(t *testing.T, f *fakeFiler, capacity int64) *evictor.Evictor {
	t.Helper()
	e := evictor.New(f, "/buckets", capacity, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	ready := make(chan struct{})
	go func() { done <- e.Run(ctx, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("Run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("evictor did not become ready")
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run: %v", err)
		}
	})

	return e
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInitialScanEvictsOverCapacity(t *testing.T) {
	t.Parallel()

	initial := make([]evictor.Entry, 0, 5)
	for i := range 5 {
		initial = append(initial, synced(fmt.Sprintf("/buckets/c/nar/%d", i)))
	}
	f := newFake(initial...)
	run(t, f, 30)

	eventually(t, "two oldest uncached", func() bool { return len(f.Uncached()) == 2 })
	got := f.Uncached()
	if !slices.Contains(got, "/buckets/c/nar/0") || !slices.Contains(got, "/buckets/c/nar/1") {
		t.Fatalf("uncached %v, want the two oldest", got)
	}
}

func TestReadsProtectHotObjects(t *testing.T) {
	t.Parallel()

	f := newFake(synced("/buckets/c/nar/hot"))
	e := run(t, f, 100)

	for range 2 {
		e.OnAccessLog(fluentin.Event{Tag: "s3.access", Record: map[string]any{
			"bucket": "c", "key": "nar/hot", "operation": "REST.GET.OBJECT", "status": int64(200),
		}})
	}
	for i := range 20 {
		f.changes <- evictor.Change{New: &evictor.Entry{Path: fmt.Sprintf("/buckets/c/nar/scan-%d", i), Size: 10, State: evictor.Synced}}
	}

	eventually(t, "scan evicted", func() bool { return len(f.Uncached()) >= 10 })
	if slices.Contains(f.Uncached(), "/buckets/c/nar/hot") {
		t.Fatalf("hot object uncached: %v", f.Uncached())
	}
}

func TestOnlySuccessfulGetsCountAsReads(t *testing.T) {
	t.Parallel()

	f := newFake(synced("/buckets/c/a"))
	e := run(t, f, 15)

	for _, rec := range []map[string]any{
		{"bucket": "c", "key": "a", "operation": "REST.PUT.OBJECT", "status": int64(200)},
		{"bucket": "c", "key": "a", "operation": "REST.GET.OBJECT", "status": int64(404)},
		{"bucket": "c", "key": "a", "operation": "REST.HEAD.OBJECT", "status": int64(200)},
	} {
		e.OnAccessLog(fluentin.Event{Tag: "s3.access", Record: rec})
	}
	f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/b", Size: 10, State: evictor.Synced}}

	eventually(t, "a uncached", func() bool { return slices.Contains(f.Uncached(), "/buckets/c/a") })
}

func TestDirtyEntriesAreKeptUntilSynced(t *testing.T) {
	t.Parallel()

	f := newFake(evictor.Entry{Path: "/buckets/c/dirty", Size: 10, State: evictor.Dirty})
	run(t, f, 20)

	for i := range 6 {
		f.changes <- evictor.Change{New: &evictor.Entry{Path: fmt.Sprintf("/buckets/c/f%d", i), Size: 10, State: evictor.Synced}}
	}
	eventually(t, "fillers evicted", func() bool { return len(f.Uncached()) >= 5 })
	if slices.Contains(f.Uncached(), "/buckets/c/dirty") {
		t.Fatal("unsynced object uncached")
	}

	// Upload finished: now it is fair game.
	f.changes <- evictor.Change{
		Old: &evictor.Entry{Path: "/buckets/c/dirty", Size: 10, State: evictor.Dirty},
		New: &evictor.Entry{Path: "/buckets/c/dirty", Size: 10, State: evictor.Synced},
	}
	for i := range 6 {
		f.changes <- evictor.Change{New: &evictor.Entry{Path: fmt.Sprintf("/buckets/c/g%d", i), Size: 10, State: evictor.Synced}}
	}
	eventually(t, "dirty uncached after sync", func() bool { return slices.Contains(f.Uncached(), "/buckets/c/dirty") })
}

func TestDeleteAndRemoteOnlyStopTracking(t *testing.T) {
	t.Parallel()

	f := newFake(synced("/buckets/c/a"), synced("/buckets/c/b"))
	run(t, f, 20)

	f.changes <- evictor.Change{Old: &evictor.Entry{Path: "/buckets/c/a", Size: 10, State: evictor.Synced}}
	f.changes <- evictor.Change{
		Old: &evictor.Entry{Path: "/buckets/c/b", Size: 10, State: evictor.Synced},
		New: &evictor.Entry{Path: "/buckets/c/b", Size: 10, State: evictor.Remote},
	}
	// Room for two more objects without evicting anything.
	f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/c", Size: 10, State: evictor.Synced}}
	f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/d", Size: 10, State: evictor.Synced}}
	time.Sleep(100 * time.Millisecond)
	if got := f.Uncached(); len(got) != 0 {
		t.Fatalf("unexpected uncache %v", got)
	}
}

func TestRenameMovesTracking(t *testing.T) {
	t.Parallel()

	f := newFake(synced("/buckets/c/old"))
	run(t, f, 20)

	f.changes <- evictor.Change{
		Old: &evictor.Entry{Path: "/buckets/c/old", Size: 10, State: evictor.Synced},
		New: &evictor.Entry{Path: "/buckets/c/new", Size: 10, State: evictor.Synced},
	}
	f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/x", Size: 10, State: evictor.Synced}}
	time.Sleep(100 * time.Millisecond)
	if got := f.Uncached(); len(got) != 0 {
		t.Fatalf("stale key still counted: uncached %v", got)
	}
}

func TestFailedUncacheIsRetried(t *testing.T) {
	t.Parallel()

	f := newFake(synced("/buckets/c/a"), synced("/buckets/c/b"))
	f.failNext["/buckets/c/a"] = true
	run(t, f, 15)

	eventually(t, "first uncache attempt failed", func() bool { return f.Pending() == 0 })

	// The next event gives the evictor another chance at the failed entry.
	f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/c", Size: 10, State: evictor.Synced}}
	eventually(t, "a eventually uncached", func() bool { return slices.Contains(f.Uncached(), "/buckets/c/a") })
}

func TestUncacheRefusedByFilerIsNotRetriedForever(t *testing.T) {
	t.Parallel()

	f := newFake(synced("/buckets/c/a"), synced("/buckets/c/b"))
	f.busy["/buckets/c/a"] = true
	run(t, f, 15)

	eventually(t, "a attempted", func() bool { return f.Attempts("/buckets/c/a") == 1 })
	f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/c", Size: 10, State: evictor.Synced}}
	eventually(t, "b uncached", func() bool { return slices.Contains(f.Uncached(), "/buckets/c/b") })
	if n := f.Attempts("/buckets/c/a"); n != 1 || slices.Contains(f.Uncached(), "/buckets/c/a") {
		t.Fatalf("refused entry attempted %d times, uncached=%v", n, f.Uncached())
	}
}

func TestSyncTriggersEviction(t *testing.T) {
	t.Parallel()

	f := newFake()
	run(t, f, 20)

	for _, p := range []string{"a", "b", "c"} {
		f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/" + p, Size: 10, State: evictor.Dirty}}
	}

	f.changes <- evictor.Change{
		Old: &evictor.Entry{Path: "/buckets/c/a", Size: 10, State: evictor.Dirty},
		New: &evictor.Entry{Path: "/buckets/c/a", Size: 10, State: evictor.Synced},
	}

	eventually(t, "a uncached once synced", func() bool { return slices.Contains(f.Uncached(), "/buckets/c/a") })

	if got := f.Uncached(); len(got) != 1 {
		t.Fatalf("uncached %v, want only a", got)
	}
}

func TestOverwriteUpdatesSize(t *testing.T) {
	t.Parallel()

	f := newFake(synced("/buckets/c/a"))
	run(t, f, 30)

	f.changes <- evictor.Change{
		Old: &evictor.Entry{Path: "/buckets/c/a", Size: 10, State: evictor.Synced},
		New: &evictor.Entry{Path: "/buckets/c/a", Size: 25, State: evictor.Synced},
	}
	f.changes <- evictor.Change{New: &evictor.Entry{Path: "/buckets/c/b", Size: 10, State: evictor.Synced}}

	eventually(t, "a uncached after growing", func() bool { return slices.Contains(f.Uncached(), "/buckets/c/a") })
}
