// Package fluentin is a minimal receiver for the fluentd forward protocol in
// Message mode, which is what the SeaweedFS S3 gateway's audit log emits
// (fluent-logger-golang). It exists so the evictor can observe object reads.
package fluentin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/vmihailenco/msgpack/v5"
)

// Event is one decoded forward-protocol message.
type Event struct {
	Tag    string
	Record map[string]any
}

const (
	minElems = 3 // [tag, time, record]
	maxElems = 4 // ... plus options, e.g. the ack chunk
)

var errUnsupported = errors.New("fluentin: unsupported message")

// Handler is called from connection goroutines and must be safe for
// concurrent use.
type Handler func(Event)

// Serve accepts connections until ctx is done or the listener fails.
func Serve(ctx context.Context, ln net.Listener, h Handler) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("fluentin: accept: %w", err)
		}

		wg.Go(func() {
			defer conn.Close()
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			defer stop()
			// A malformed peer only costs its own connection.
			_ = serveConn(conn, h)
		})
	}
}

func serveConn(conn net.Conn, h Handler) error {
	dec := msgpack.NewDecoder(conn)
	// Decode every integer as int64 so consumers need no width switch.
	dec.UseLooseInterfaceDecoding(true)
	enc := msgpack.NewEncoder(conn)

	for {
		ev, chunk, err := readMessage(dec)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		h(ev)

		if chunk != "" {
			if err := enc.Encode(map[string]string{"ack": chunk}); err != nil {
				return fmt.Errorf("fluentin: send ack: %w", err)
			}
		}
	}
}

// readMessage decodes [tag, time, record, option?]. The time is skipped
// because it may be an EventTime extension and the evictor uses arrival time.
func readMessage(dec *msgpack.Decoder) (Event, string, error) {
	var ev Event

	n, err := dec.DecodeArrayLen()
	if err != nil {
		return ev, "", fmt.Errorf("fluentin: message header: %w", err)
	}

	if n < minElems || n > maxElems {
		return ev, "", fmt.Errorf("%w: %d elements", errUnsupported, n)
	}

	if ev.Tag, err = dec.DecodeString(); err != nil {
		return ev, "", fmt.Errorf("fluentin: tag: %w", err)
	}

	if err = dec.Skip(); err != nil {
		return ev, "", fmt.Errorf("fluentin: time: %w", err)
	}

	if ev.Record, err = dec.DecodeMap(); err != nil {
		return ev, "", fmt.Errorf("fluentin: record: %w", err)
	}

	if n == maxElems {
		opt, err := dec.DecodeMap()
		if err != nil {
			return ev, "", fmt.Errorf("fluentin: option: %w", err)
		}

		chunk, _ := opt["chunk"].(string)

		return ev, chunk, nil
	}

	return ev, "", nil
}
