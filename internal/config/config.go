// Package config reads the evictor's settings from flags and the environment.
//
// Every flag has an environment variable (--log-level -> SEAWEED_EVICTOR_LOG_LEVEL)
// so the process can be configured without a command line, e.g. in a container
// or systemd unit. A flag on the command line wins over the environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/dustin/go-humanize"
)

const envPrefix = "SEAWEED_EVICTOR_"

var (
	errCapacityRequired = errors.New("capacity is required: set --capacity or " + envPrefix + "CAPACITY")
	errCapacityPositive = errors.New("capacity must be greater than zero")
	errNotASize         = errors.New("not a size like 10GiB or 500000000")
)

// Config is the validated configuration.
type Config struct {
	// Filer is the filer's gRPC address (HTTP port + 10000).
	Filer string
	// Root is the filer directory to manage.
	Root string
	// Capacity is the local cache budget in bytes.
	Capacity int64
	// FluentListen receives the S3 gateway's audit log. Empty disables it.
	FluentListen string
	LogLevel     slog.Level
}

// EnvName returns the environment variable that backs a flag.
func EnvName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// Parse reads args (without the program name). getenv is os.Getenv in
// production. Usage and parse errors are written to out.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	var (
		cfg      Config
		capacity = "" // parsed below so both flag and env accept "10GiB"
		level    = "info"
	)

	fs := flag.NewFlagSet("seaweed-evictor", flag.ContinueOnError)
	fs.SetOutput(out)

	fs.StringVar(&cfg.Filer, "filer", "localhost:18888", "filer gRPC address (HTTP port + 10000)")
	fs.StringVar(&cfg.Root, "root", "/buckets", "filer directory to manage")
	fs.StringVar(&capacity, "capacity", "", "local cache budget, e.g. 10GiB or 500000000 (required)")
	fs.StringVar(&cfg.FluentListen, "fluent-listen", "127.0.0.1:24224",
		"listen address for the S3 gateway audit log; set it empty to disable read tracking")
	fs.StringVar(&level, "log-level", level, "log level: debug, info, warn or error")

	fs.Usage = func() { usage(fs, out) }

	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}

	fromEnv, err := fillFromEnv(fs, getenv)
	if err != nil {
		return Config{}, err
	}

	if capacity == "" {
		return Config{}, errCapacityRequired
	}

	bytes, err := humanize.ParseBytes(capacity)
	if err != nil {
		return Config{}, fmt.Errorf("%s %q: %w", source("capacity", fromEnv), capacity, errNotASize)
	}

	if bytes == 0 || bytes > 1<<62 {
		return Config{}, errCapacityPositive
	}

	cfg.Capacity = int64(bytes)

	if err := cfg.LogLevel.UnmarshalText([]byte(level)); err != nil {
		return Config{}, fmt.Errorf("%s %q: %w", source("log-level", fromEnv), level, err)
	}

	return cfg, nil
}

// fillFromEnv sets every flag that was not given on the command line from its
// environment variable and reports which flags it set. An unset or empty
// variable keeps the default, so an empty one cannot silently turn a feature
// off; only an explicit empty flag does that.
func fillFromEnv(fs *flag.FlagSet, getenv func(string) string) (map[string]bool, error) {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	fromEnv := map[string]bool{}

	var err error

	fs.VisitAll(func(f *flag.Flag) {
		v := getenv(EnvName(f.Name))
		if err != nil || given[f.Name] || v == "" {
			return
		}

		if setErr := f.Value.Set(v); setErr != nil {
			err = fmt.Errorf("%s: %w", EnvName(f.Name), setErr)

			return
		}

		fromEnv[f.Name] = true
	})

	return fromEnv, err
}

// source names where a value came from, for error messages.
func source(flagName string, fromEnv map[string]bool) string {
	if fromEnv[flagName] {
		return EnvName(flagName)
	}

	return "--" + flagName
}

// usage prints flags with two dashes, which is what users of GNU-style tools
// expect. The flag package accepts both spellings when parsing.
func usage(fs *flag.FlagSet, out io.Writer) {
	fmt.Fprintf(out, "Usage: seaweed-evictor --capacity SIZE [flags]\n\n")
	fmt.Fprintf(out, "Keeps a SeaweedFS remote-mounted cache within a size limit using S3-FIFO.\n")
	fmt.Fprintf(out, "Every flag can also be set through its environment variable; flags win.\n\n")
	fmt.Fprintf(out, "Flags:\n")

	fs.VisitAll(func(f *flag.Flag) {
		def := ""
		if f.DefValue != "" {
			def = fmt.Sprintf(" (default %q)", f.DefValue)
		}

		fmt.Fprintf(out, "  --%s\n    \t%s%s\n    \t[env: %s]\n", f.Name, f.Usage, def, EnvName(f.Name))
	})
}
