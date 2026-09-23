package fluentin_test

import (
	"net"
	"testing"
	"time"

	"github.com/Mic92/seaweed-evictor/internal/fluentin"
	"github.com/fluent/fluent-logger-golang/fluent"
)

// accessLog mirrors the subset of seaweedfs' s3err.AccessLog we consume.
type accessLog struct {
	Bucket    string `json:"bucket"              msg:"bucket"`
	Operation string `json:"operation,omitempty" msg:"operation"`
	Key       string `json:"key,omitempty"       msg:"key"`
	Status    int    `json:"status,omitempty"    msg:"status"`
}

func listen(t *testing.T) net.Listener {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	return ln
}

func portOf(t *testing.T, ln net.Listener) int {
	t.Helper()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected address type %T", ln.Addr())
	}

	return addr.Port
}

func start(t *testing.T) (int, <-chan fluentin.Event) {
	t.Helper()

	ln := listen(t)
	ch := make(chan fluentin.Event, 16)
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = fluentin.Serve(t.Context(), ln, func(e fluentin.Event) { ch <- e })
	}()
	// t.Context is cancelled before cleanups run.
	t.Cleanup(func() { <-done })

	return portOf(t, ln), ch
}

func recv(t *testing.T, ch <-chan fluentin.Event) fluentin.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for event")

		return fluentin.Event{}
	}
}

func TestReceivesGatewayAccessLog(t *testing.T) {
	t.Parallel()

	port, ch := start(t)

	// Same construction as weed/s3api/s3err.InitAuditLog.
	f, err := fluent.New(fluent.Config{FluentPort: port, FluentHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rec := accessLog{Bucket: "cache", Operation: "REST.GET.OBJECT", Key: "objects/abc.bin", Status: 200}
	if err := f.Post("s3.access", rec); err != nil {
		t.Fatal(err)
	}

	e := recv(t, ch)
	if e.Tag != "s3.access" {
		t.Errorf("tag = %q", e.Tag)
	}
	if e.Record["bucket"] != "cache" || e.Record["operation"] != "REST.GET.OBJECT" || e.Record["key"] != "objects/abc.bin" {
		t.Errorf("record = %v", e.Record)
	}
}

func TestSubSecondTimeAndAck(t *testing.T) {
	t.Parallel()

	port, ch := start(t)

	f, err := fluent.New(fluent.Config{
		FluentPort:         port,
		FluentHost:         "127.0.0.1",
		SubSecondPrecision: true,
		RequestAck:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// With RequestAck Post blocks until the server acknowledged the chunk.
	if err := f.Post("s3.access", accessLog{Key: "k"}); err != nil {
		t.Fatal(err)
	}
	if e := recv(t, ch); e.Record["key"] != "k" {
		t.Errorf("record = %v", e.Record)
	}
}

func TestManyEventsOnOneConnection(t *testing.T) {
	t.Parallel()

	port, ch := start(t)

	f, err := fluent.New(fluent.Config{FluentPort: port, FluentHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for range 10 {
		if err := f.Post("s3.access", accessLog{Key: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		recv(t, ch)
	}
}

func TestGarbageClosesConnectionWithoutStoppingServer(t *testing.T) {
	t.Parallel()

	ln := listen(t)
	ch := make(chan fluentin.Event, 1)

	go func() { _ = fluentin.Serve(t.Context(), ln, func(e fluentin.Event) { ch <- e }) }()

	var d net.Dialer

	c, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	_, _ = c.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	_ = c.Close()

	f, err := fluent.New(fluent.Config{FluentPort: portOf(t, ln), FluentHost: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := f.Post("s3.access", accessLog{Key: "still-works"}); err != nil {
		t.Fatal(err)
	}

	if e := recv(t, ch); e.Record["key"] != "still-works" {
		t.Errorf("record = %v", e.Record)
	}
}
