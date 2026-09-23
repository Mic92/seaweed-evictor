// Package seaweed implements evictor.Filer on top of the SeaweedFS filer gRPC API.
package seaweed

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/Mic92/seaweed-evictor/internal/evictor"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Filer struct {
	conn   *grpc.ClientConn
	client filer_pb.SeaweedFilerClient
}

// Dial connects to the filer's gRPC port (HTTP port + 10000 by default).
func Dial(addr string) (*Filer, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("dial filer %s: %w", addr, err)
	}

	return &Filer{conn: conn, client: filer_pb.NewSeaweedFilerClient(conn)}, nil
}

// Close releases the connection.
func (f *Filer) Close() error {
	if err := f.conn.Close(); err != nil {
		return fmt.Errorf("close filer connection: %w", err)
	}

	return nil
}

// classify maps a filer entry to the evictor's view. Entries without a
// RemoteEntry have never been uploaded, so they count as dirty.
func classify(e *filer_pb.Entry) evictor.State {
	re := e.GetRemoteEntry()
	switch {
	case re == nil:
		return evictor.Dirty
	case re.GetLastLocalSyncTsNs() == 0:
		return evictor.Remote
	case re.GetLastLocalSyncTsNs()/1e9 < e.GetAttributes().GetMtime():
		return evictor.Dirty
	default:
		return evictor.Synced
	}
}

func toEntry(p string, e *filer_pb.Entry) *evictor.Entry {
	size := min(e.GetAttributes().GetFileSize(), math.MaxInt64)

	return &evictor.Entry{Path: p, Size: int64(size), State: classify(e)}
}

// WithFilerClient and the two methods after it implement filer_pb.FilerClient,
// which TraverseBfs needs. Only the gRPC client is used.
func (f *Filer) WithFilerClient(_ bool, fn func(filer_pb.SeaweedFilerClient) error) error {
	return fn(f.client)
}

//nolint:revive // name is fixed by filer_pb.FilerClient
func (f *Filer) AdjustedUrl(*filer_pb.Location) string { return "" }

func (f *Filer) GetDataCenter() string { return "" }

// Walk lists every file below root that holds local data. TraverseBfs calls
// back from several goroutines, so calls to fn are serialized here.
func (f *Filer) Walk(ctx context.Context, root string, fn func(evictor.Entry)) (int64, error) {
	since := time.Now().UnixNano()

	var mu sync.Mutex

	err := filer_pb.TraverseBfs(ctx, f, util.FullPath(root), func(dir util.FullPath, e *filer_pb.Entry) error {
		if e.GetIsDirectory() {
			return nil
		}

		if en := toEntry(path.Join(string(dir), e.GetName()), e); en.State != evictor.Remote {
			mu.Lock()
			fn(*en)
			mu.Unlock()
		}

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk %s: %w", root, err)
	}

	return since, nil
}

func (f *Filer) Subscribe(ctx context.Context, root string, sinceNs int64, fn func(evictor.Change)) error {
	stream, err := f.client.SubscribeMetadata(ctx, &filer_pb.SubscribeMetadataRequest{
		ClientName: "seaweed-evictor",
		PathPrefix: root,
		SinceNs:    sinceNs,
		// The filer treats a subscriber without an id as gone as soon as it
		// has caught up and ends the stream.
		ClientId:    newClientID(),
		ClientEpoch: 1,
	})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("subscribe: %w", err)
		}

		if c, ok := toChange(resp); ok {
			fn(c)
		}
	}
}

// newClientID returns a random positive id; zero means "no id" to the filer.
func newClientID() int32 {
	var b [4]byte

	if _, err := rand.Read(b[:]); err != nil {
		return 1
	}

	return int32(binary.BigEndian.Uint32(b[:])>>1) | 1
}

func toChange(resp *filer_pb.SubscribeMetadataResponse) (evictor.Change, bool) {
	n := resp.GetEventNotification()
	oldE, newE := n.GetOldEntry(), n.GetNewEntry()
	if oldE.GetIsDirectory() || newE.GetIsDirectory() {
		return evictor.Change{}, false
	}
	var c evictor.Change
	if oldE != nil {
		c.Old = toEntry(path.Join(resp.GetDirectory(), oldE.GetName()), oldE)
	}
	if newE != nil {
		dir := resp.GetDirectory()
		if n.GetNewParentPath() != "" {
			dir = n.GetNewParentPath()
		}
		c.New = toEntry(path.Join(dir, newE.GetName()), newE)
	}

	return c, c.Old != nil || c.New != nil
}

// Uncache drops the chunks of a synced entry, like `weed shell remote.uncache`.
// It looks the entry up again first so a write that raced with eviction wins.
// The window between lookup and update remains, as in the shell command.
func (f *Filer) Uncache(ctx context.Context, p string) (bool, error) {
	dir, name := path.Split(p)
	dir = path.Clean(dir)
	resp, err := f.client.LookupDirectoryEntry(ctx, &filer_pb.LookupDirectoryEntryRequest{Directory: dir, Name: name})
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("lookup %s: %w", p, err)
	}
	e := resp.GetEntry()
	if classify(e) != evictor.Synced {
		return false, nil
	}
	e.RemoteEntry.LastLocalSyncTsNs = 0
	e.Chunks = nil
	e.Content = nil
	if _, err := f.client.UpdateEntry(ctx, &filer_pb.UpdateEntryRequest{Directory: dir, Entry: e}); err != nil {
		return false, fmt.Errorf("update %s: %w", p, err)
	}

	return true, nil
}

func isNotFound(err error) bool {
	return errors.Is(err, filer_pb.ErrNotFound) || strings.Contains(err.Error(), "no entry is found")
}
