package seaweed_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mic92/seaweed-evictor/internal/evictor"
	"github.com/Mic92/seaweed-evictor/internal/fluentin"
	"github.com/Mic92/seaweed-evictor/internal/seaweed"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	accessKey = "test"
	secretKey = "testtest12345"
	region    = "us-east-1"

	objectSize = 1 << 20
	capacity   = 5 * objectSize
	numObjects = 12
	hotReads   = 3

	grpcOffset = 10000
	waitLimit  = 90 * time.Second
)

// node is one `weed mini` process: master, volume, filer and S3 gateway.
type node struct {
	filerHTTP, s3, master int
}

// ports hands out ports from a range owned by one test. Ranges of different
// tests do not overlap and lie below the kernel's ephemeral range, so parallel
// tests cannot be given the same port between our probe and weed binding it.
// weed also needs port+grpcOffset, which is why both are probed.
type ports struct {
	t    *testing.T
	next int
}

const (
	portRangeStart = 12000
	portRangeSize  = 64
	maxSlots       = 3
)

// newPorts returns the allocator for a test; slot must be unique per test.
func newPorts(t *testing.T, slot int) *ports {
	t.Helper()

	return &ports{t: t, next: portRangeStart + slot*portRangeSize}
}

func (p *ports) take() int {
	p.t.Helper()

	var lc net.ListenConfig

	for range portRangeSize {
		port := p.next
		p.next++

		if p.next > portRangeStart+portRangeSize*maxSlots {
			p.t.Fatal("port range exhausted")
		}

		if free(p.t, &lc, port) && free(p.t, &lc, port+grpcOffset) {
			return port
		}
	}

	p.t.Fatal("no free port pair")

	return 0
}

func free(t *testing.T, lc *net.ListenConfig, port int) bool {
	t.Helper()

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}

	_ = ln.Close()

	return true
}

func startNode(t *testing.T, p *ports, name string, extra ...string) node {
	t.Helper()

	n := node{filerHTTP: p.take(), s3: p.take(), master: p.take()}
	volume, admin, webdav := p.take(), p.take(), p.take()

	args := append([]string{
		"mini",
		"-dir=" + t.TempDir(),
		"-ip=127.0.0.1", "-ip.bind=127.0.0.1",
		"-master.port=" + strconv.Itoa(n.master),
		"-volume.port=" + strconv.Itoa(volume),
		"-filer.port=" + strconv.Itoa(n.filerHTTP),
		"-s3.port=" + strconv.Itoa(n.s3),
		"-admin.port=" + strconv.Itoa(admin),
		"-webdav.port=" + strconv.Itoa(webdav),
		"-s3.port.iceberg=0", "-s3.port.lance=0",
		"-master.telemetry=false", "-admin.ui=false",
	}, extra...)

	runWeed(t, name, args, nil)

	return n
}

// runWeed starts weed in the background and prints the tail of its log if the
// test fails.
func runWeed(t *testing.T, name string, args []string, stdin io.Reader) {
	t.Helper()

	logPath := filepath.Join(t.TempDir(), name+".log")

	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), "weed", args...)
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+accessKey,
		"AWS_SECRET_ACCESS_KEY="+secretKey,
		"HOME="+t.TempDir(),
	)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start weed %s: %v", name, err)
	}

	t.Cleanup(func() {
		_ = cmd.Wait() // killed by the test context
		_ = logFile.Close()

		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("--- %s log (tail)\n%s", name, tail(string(data), 400))
		}
	})
}

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}

	return strings.Join(parts, "\n")
}

func s3Client(t *testing.T, port int) *minio.Client {
	t.Helper()

	c, err := minio.New("127.0.0.1:"+strconv.Itoa(port), &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Region: region,
	})
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

func put(t *testing.T, c *minio.Client, bucket, key string, data []byte) {
	t.Helper()

	_, err := c.PutObject(t.Context(), bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		t.Fatalf("put %s/%s: %v", bucket, key, err)
	}
}

func get(t *testing.T, c *minio.Client, bucket, key string) []byte {
	t.Helper()

	obj, err := c.GetObject(t.Context(), bucket, key, minio.GetObjectOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", bucket, key, err)
	}
	defer obj.Close()

	data, err := io.ReadAll(obj)
	if err != nil {
		t.Fatalf("read %s/%s: %v", bucket, key, err)
	}

	return data
}

func randomBytes(t *testing.T) []byte {
	t.Helper()

	b := make([]byte, objectSize)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}

	return b
}

// cachedEntries lists what the local filer holds below the mount, keyed by path.
func cachedEntries(t *testing.T, f *seaweed.Filer) map[string]evictor.Entry {
	t.Helper()

	out := map[string]evictor.Entry{}

	if _, err := f.Walk(t.Context(), "/buckets/cache", func(e evictor.Entry) { out[e.Path] = e }); err != nil {
		t.Fatalf("walk: %v", err)
	}

	return out
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))

	return len(p), nil
}

func TestEvictionAgainstSeaweedFS(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("weed"); err != nil {
		t.Skip("weed not in PATH; run inside `nix develop`")
	}

	p := newPorts(t, 0)
	fluentPort := p.take()

	// "remote" plays the upstream S3 bucket, "local" is the cache.
	remote := startNode(t, p, "remote", "-bucket=upstream")
	local := startNode(t, p, "local",
		"-filer.allowUntrustedRemoteEndpoints", "-s3.allowUntrustedRemoteEndpoints",
		"-s3.auditLogConfig="+writeAuditConfig(t, fluentPort),
	)

	up, cache := s3Client(t, remote.s3), s3Client(t, local.s3)

	eventually(t, "remote S3 gateway", func() bool {
		ok, err := up.BucketExists(t.Context(), "upstream")

		return err == nil && ok
	})

	mountRemote(t, local, remote)

	eventually(t, "local S3 gateway", func() bool {
		_, err := cache.ListBuckets(t.Context())

		return err == nil
	})

	runWeed(t, "remote-sync", []string{
		"filer.remote.sync", "-filer=127.0.0.1:" + strconv.Itoa(local.filerHTTP), "-dir=/buckets/cache",
	}, nil)

	filer, err := seaweed.Dial("127.0.0.1:" + strconv.Itoa(local.filerHTTP+grpcOffset))
	if err != nil {
		t.Fatal(err)
	}

	// Registered before startEvictor's cleanup, so the evictor stops first.
	t.Cleanup(func() { _ = filer.Close() })

	var hotHits atomic.Int64

	ev := evictor.New(filer, "/buckets", capacity, slog.New(slog.NewTextHandler(testLogWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	startAuditReceiver(t, fluentPort, ev, &hotHits)
	startEvictor(t, ev)

	written := map[string][]byte{}
	upload := func(key string) {
		data := randomBytes(t)
		written[key] = data
		put(t, cache, "cache", key, data)
	}
	inUpstream := func(key string) bool {
		_, err := up.StatObject(t.Context(), "upstream", key, minio.StatObjectOptions{})

		return err == nil
	}

	// One object is read repeatedly, so S3-FIFO must keep it while the
	// one-hit wonders that follow are evicted.
	upload("hot")
	eventually(t, "hot object uploaded upstream", func() bool { return inUpstream("hot") })

	for range hotReads {
		if !bytes.Equal(get(t, cache, "cache", "hot"), written["hot"]) {
			t.Fatal("hot object corrupted")
		}
	}

	eventually(t, "evictor saw the reads", func() bool { return hotHits.Load() >= hotReads })

	for i := range numObjects {
		upload(fmt.Sprintf("obj-%02d", i))
	}

	for key := range written {
		eventually(t, key+" uploaded upstream", func() bool { return inUpstream(key) })
	}

	t.Cleanup(func() {
		if t.Failed() {
			ctx := context.WithoutCancel(t.Context())
			out := map[string]evictor.Entry{}
			_, _ = filer.Walk(ctx, "/buckets/cache", func(e evictor.Entry) { out[e.Path] = e })
			for p, e := range out {
				t.Logf("walk: %s size=%d state=%d", p, e.Size, e.State)
			}
		}
	})

	eventually(t, "local cache within capacity", func() bool {
		var total int64

		for _, e := range cachedEntries(t, filer) {
			total += e.Size
		}

		return total <= capacity
	})

	cached := cachedEntries(t, filer)
	if _, ok := cached["/buckets/cache/hot"]; !ok {
		t.Errorf("hot object was evicted; cached: %v", keys(cached))
	}

	if len(cached) >= len(written) {
		t.Errorf("nothing was evicted: %d cached of %d", len(cached), len(written))
	}

	// Evicted objects must still be served, fetched back from upstream.
	for key, want := range written {
		if !bytes.Equal(get(t, cache, "cache", key), want) {
			t.Errorf("%s differs after eviction", key)
		}
	}
}

func keys(m map[string]evictor.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}

func writeAuditConfig(t *testing.T, port int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "audit.json")
	cfg := fmt.Sprintf(`{"fluent_host": "127.0.0.1", "fluent_port": %d}`, port)

	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// mountRemote wires local's /buckets/cache to remote's "upstream" bucket.
func mountRemote(t *testing.T, local, remote node) {
	t.Helper()

	script := fmt.Sprintf(
		"remote.configure -name=up -type=s3 -s3.access_key=%s -s3.secret_key=%s -s3.region=%s "+
			"-s3.endpoint=http://127.0.0.1:%d -s3.force_path_style=true\n"+
			"remote.mount -dir=/buckets/cache -remote=up/upstream\nexit\n",
		accessKey, secretKey, region, remote.s3)

	ctx, cancel := context.WithTimeout(t.Context(), waitLimit)
	defer cancel()

	args := []string{"shell", "-master=127.0.0.1:" + strconv.Itoa(local.master)}
	cmd := exec.CommandContext(ctx, "weed", args...)
	cmd.Stdin = strings.NewReader(script)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("weed shell: %v\n%s", err, out)
	}

	t.Logf("weed shell:\n%s", out)
}

func startAuditReceiver(t *testing.T, port int, ev *evictor.Evictor, hotHits *atomic.Int64) {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = fluentin.Serve(t.Context(), ln, func(e fluentin.Event) {
			ev.OnAccessLog(e)

			// Counted after the evictor processed it.
			if e.Record["key"] == "hot" && e.Record["operation"] == "REST.GET.OBJECT" {
				hotHits.Add(1)
			}
		})
	}()
	t.Cleanup(func() { <-done })
}

func startEvictor(t *testing.T, ev *evictor.Evictor) {
	t.Helper()

	ready := make(chan struct{})
	done := make(chan error, 1)

	go func() { done <- ev.Run(t.Context(), func() { close(ready) }) }()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("evictor exited early: %v", err)
	case <-time.After(waitLimit):
		t.Fatal("evictor did not finish its initial scan")
	}

	t.Cleanup(func() {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("evictor: %v", err)
		}
	})
}

// The filer ends a subscription without a client id as soon as it has caught
// up, so Subscribe must stay open and deliver writes that happen later.
func TestSubscribeStaysOpen(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("weed"); err != nil {
		t.Skip("weed not in PATH; run inside `nix develop`")
	}

	n := startNode(t, newPorts(t, 1), "subscribe", "-bucket=cache")
	c := s3Client(t, n.s3)

	eventually(t, "S3 gateway", func() bool {
		ok, err := c.BucketExists(t.Context(), "cache")

		return err == nil && ok
	})

	filer, err := seaweed.Dial("127.0.0.1:" + strconv.Itoa(n.filerHTTP+grpcOffset))
	if err != nil {
		t.Fatal(err)
	}
	defer filer.Close()

	changes := make(chan evictor.Change, 16)
	errs := make(chan error, 1)

	go func() {
		errs <- filer.Subscribe(t.Context(), "/buckets", time.Now().UnixNano(), func(c evictor.Change) { changes <- c })
	}()

	select {
	case err := <-errs:
		t.Fatalf("subscription ended while idle: %v", err)
	case <-time.After(2 * time.Second):
	}

	put(t, c, "cache", "late-object", []byte("x"))

	deadline := time.After(waitLimit)

	for {
		select {
		case ch := <-changes:
			if ch.New != nil && ch.New.Path == "/buckets/cache/late-object" {
				return
			}
		case err := <-errs:
			t.Fatalf("subscription ended before the write arrived: %v", err)
		case <-deadline:
			t.Fatal("no event for the late write")
		}
	}
}

func TestWalkFindsNestedObjects(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("weed"); err != nil {
		t.Skip("weed not in PATH; run inside `nix develop`")
	}

	n := startNode(t, newPorts(t, 2), "walk", "-bucket=cache")
	c := s3Client(t, n.s3)

	eventually(t, "S3 gateway", func() bool {
		ok, err := c.BucketExists(t.Context(), "cache")

		return err == nil && ok
	})

	want := []string{"top", "nar/one", "nar/deep/two", "nar/deep/er/three"}
	for _, key := range want {
		put(t, c, "cache", key, []byte(key))
	}

	filer, err := seaweed.Dial("127.0.0.1:" + strconv.Itoa(n.filerHTTP+grpcOffset))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = filer.Close() })

	got := cachedEntries(t, filer)
	for _, key := range want {
		e, ok := got["/buckets/cache/"+key]
		if !ok {
			t.Errorf("walk missed %s; found %v", key, keys(got))

			continue
		}

		if e.Size != int64(len(key)) {
			t.Errorf("%s size = %d, want %d", key, e.Size, len(key))
		}
	}
}
