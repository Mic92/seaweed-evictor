// Package evictor keeps the local part of a SeaweedFS remote-mounted bucket
// within a byte budget using S3-FIFO. Reads are learned from the S3 gateway's
// audit log because the filer emits no metadata event for a cache hit.
package evictor

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/Mic92/seaweed-evictor/internal/fluentin"
	"github.com/Mic92/seaweed-evictor/s3fifo"
)

// State describes where the bytes of a filer entry live.
type State int

const (
	// Remote entries have no local chunks; nothing to evict.
	Remote State = iota
	// Synced entries are cached locally and identical to the remote object.
	Synced
	// Dirty entries hold local data that has not been uploaded yet.
	Dirty
)

type Entry struct {
	Path  string
	Size  int64
	State State
}

// Change is a filer metadata event: Old only is a delete, New only a create,
// both an update or rename.
type Change struct {
	Old, New *Entry
}

type Filer interface {
	// Walk lists all files below root and returns a timestamp (ns) from
	// before the listing started, to resume Subscribe without gaps.
	Walk(ctx context.Context, root string, fn func(Entry)) (sinceNs int64, err error)
	// Subscribe blocks and delivers changes below root until ctx is done.
	Subscribe(ctx context.Context, root string, sinceNs int64, fn func(Change)) error
	// Uncache drops the local copy. It reports false when the entry must
	// stay, e.g. because it changed since it was seen.
	Uncache(ctx context.Context, path string) (bool, error)
}

type Evictor struct {
	filer Filer
	root  string
	log   *slog.Logger

	mu    sync.Mutex
	cache *s3fifo.Cache[string]
	retry []string
}

func New(f Filer, root string, capacityBytes int64, log *slog.Logger) *Evictor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	return &Evictor{
		filer: f,
		root:  strings.TrimSuffix(root, "/"),
		log:   log,
		cache: s3fifo.New[string](int(capacityBytes)),
	}
}

// Run scans, calls ready once live, then follows changes until ctx is done.
func (e *Evictor) Run(ctx context.Context, ready func()) error {
	var victims []string
	since, err := e.filer.Walk(ctx, e.root, func(en Entry) {
		victims = append(victims, e.apply(Change{New: &en})...)
	})
	if err != nil {
		return fmt.Errorf("initial scan: %w", err)
	}

	e.uncache(ctx, victims)

	if ready != nil {
		ready()
	}
	err = e.filer.Subscribe(ctx, e.root, since, func(c Change) {
		e.uncache(ctx, append(e.takeRetries(), e.apply(c)...))
	})
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	return nil
}

// OnAccessLog turns successful object reads from the S3 gateway audit log
// into S3-FIFO hits.
func (e *Evictor) OnAccessLog(ev fluentin.Event) {
	if op, _ := ev.Record["operation"].(string); op != "REST.GET.OBJECT" {
		return
	}
	if status, _ := ev.Record["status"].(int64); status != http.StatusOK && status != http.StatusPartialContent {
		return
	}
	bucket, _ := ev.Record["bucket"].(string)
	key, _ := ev.Record["key"].(string)
	if bucket == "" || key == "" {
		return
	}
	e.mu.Lock()
	e.cache.Access(e.root + "/" + bucket + "/" + strings.TrimPrefix(key, "/"))
	e.mu.Unlock()
}

func (e *Evictor) apply(c Change) []string {
	e.mu.Lock()
	defer e.mu.Unlock()

	if c.Old != nil && (c.New == nil || c.New.Path != c.Old.Path || c.New.State == Remote) {
		// A victim's own "now remote" event arrives here too; keeping its
		// ghost entry is what lets a quick re-read skip the small queue.
		if e.cache.Contains(c.Old.Path) {
			e.cache.Remove(c.Old.Path)
		}
	}
	if c.New == nil || c.New.State == Remote {
		return nil
	}

	if c.New.State == Dirty {
		return e.cache.InsertPinned(c.New.Path, int(c.New.Size))
	}

	// Always insert: an overwrite changes the size, and a finished upload
	// unpins the entry, which is what makes an over-capacity cache evictable.
	victims := e.cache.Insert(c.New.Path, int(c.New.Size))
	e.cache.Unpin(c.New.Path)

	return append(victims, e.cache.Evict()...)
}

func (e *Evictor) takeRetries() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.retry
	e.retry = nil

	return r
}

func (e *Evictor) uncache(ctx context.Context, paths []string) {
	for _, p := range paths {
		ok, err := e.filer.Uncache(ctx, p)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			e.log.Warn("uncache failed, will retry", "path", p, "err", err)
			e.mu.Lock()
			e.retry = append(e.retry, p)
			e.mu.Unlock()
		case !ok:
			// The entry changed; its next event re-registers it.
			e.log.Debug("uncache refused", "path", p)
		default:
			e.log.Debug("uncached", "path", p)
		}
	}
}
