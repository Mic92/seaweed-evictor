// seaweed-evictor bounds the local cache of a SeaweedFS remote-mounted S3 bucket
// with S3-FIFO. Point `weed s3 -auditLogConfig` at --fluent-listen so it can
// see reads. See --help for the settings and their environment variables.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Mic92/seaweed-evictor/internal/config"
	"github.com/Mic92/seaweed-evictor/internal/evictor"
	"github.com/Mic92/seaweed-evictor/internal/fluentin"
	"github.com/Mic92/seaweed-evictor/internal/seaweed"
)

func main() {
	err := run()

	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
	default:
		slog.Error("seaweed-evictor", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Parse(os.Args[1:], os.Getenv, os.Stdout)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// Logs are an event stream: write them to stdout unbuffered and let the
	// environment (journald, a container runtime) route them.
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	filer, err := seaweed.Dial(cfg.Filer)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer filer.Close()

	ev := evictor.New(filer, cfg.Root, cfg.Capacity, log)

	if cfg.FluentListen != "" {
		var lc net.ListenConfig

		ln, err := lc.Listen(ctx, "tcp", cfg.FluentListen)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}

		go func() {
			if err := fluentin.Serve(ctx, ln, ev.OnAccessLog); err != nil {
				log.Error("fluent receiver", "err", err)
			}
		}()
	}

	log.Info("starting", "filer", cfg.Filer, "root", cfg.Root, "capacity_bytes", cfg.Capacity)

	if err := ev.Run(ctx, func() { log.Info("initial scan done", "root", cfg.Root) }); err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}
