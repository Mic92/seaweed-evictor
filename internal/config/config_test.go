package config_test

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"strings"
	"testing"

	"github.com/Mic92/seaweed-evictor/internal/config"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseFlags(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse(
		[]string{"--filer", "filer:18888", "--root=/data", "--capacity", "10GiB", "--fluent-listen=", "--log-level", "debug"},
		env(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	want := config.Config{
		Filer:        "filer:18888",
		Root:         "/data",
		Capacity:     10 << 30,
		FluentListen: "",
		LogLevel:     slog.LevelDebug,
	}
	if cfg != want {
		t.Errorf("got %+v, want %+v", cfg, want)
	}
}

func TestParseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]string{"--capacity", "1000000"}, env(nil), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Filer != "localhost:18888" || cfg.Root != "/buckets" ||
		cfg.FluentListen != "127.0.0.1:24224" || cfg.LogLevel != slog.LevelInfo || cfg.Capacity != 1000000 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestEnvFillsInAndFlagsWin(t *testing.T) {
	t.Parallel()

	e := env(map[string]string{
		"SEAWEED_EVICTOR_CAPACITY":      "2GiB",
		"SEAWEED_EVICTOR_FILER":         "from-env:1",
		"SEAWEED_EVICTOR_FLUENT_LISTEN": "0.0.0.0:9999",
	})

	cfg, err := config.Parse([]string{"--filer", "from-flag:2"}, e, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Filer != "from-flag:2" {
		t.Errorf("flag should beat env, got filer %q", cfg.Filer)
	}

	if cfg.Capacity != 2<<30 {
		t.Errorf("capacity from env = %d", cfg.Capacity)
	}

	if cfg.FluentListen != "0.0.0.0:9999" {
		t.Errorf("fluent listen from env = %q", cfg.FluentListen)
	}
}

func TestEmptyEnvDoesNotDisableFluent(t *testing.T) {
	t.Parallel()

	// An unset variable must keep the default; only an explicit empty flag
	// turns read tracking off.
	cfg, err := config.Parse([]string{"--capacity", "1MB"}, env(map[string]string{"SEAWEED_EVICTOR_FLUENT_LISTEN": ""}), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}

	if cfg.FluentListen == "" {
		t.Error("empty env var disabled the fluent receiver")
	}
}

func TestInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		wantErr string
	}{
		{"missing capacity", nil, nil, "SEAWEED_EVICTOR_CAPACITY"},
		{"zero capacity", []string{"--capacity", "0"}, nil, "capacity"},
		{"bad capacity", []string{"--capacity", "lots"}, nil, "capacity"},
		{"bad level", []string{"--capacity", "1MB", "--log-level", "loud"}, nil, "log-level"},
		{"bad env names the variable", nil, map[string]string{"SEAWEED_EVICTOR_CAPACITY": "lots"}, "SEAWEED_EVICTOR_CAPACITY"},
		{"unknown flag", []string{"--capacity", "1MB", "--nope"}, nil, "nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Parse(tt.args, env(tt.env), &bytes.Buffer{})
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer

	_, err := config.Parse([]string{"--help"}, env(nil), &out)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("got %v, want flag.ErrHelp", err)
	}

	help := out.String()

	for _, want := range []string{
		"--filer", "--root", "--capacity", "--fluent-listen", "--log-level",
		"SEAWEED_EVICTOR_FILER", "SEAWEED_EVICTOR_CAPACITY", "SEAWEED_EVICTOR_LOG_LEVEL",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help lacks %q:\n%s", want, help)
		}
	}

	for line := range strings.SplitSeq(help, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-") && !strings.HasPrefix(strings.TrimSpace(line), "--") {
			t.Errorf("help uses a single dash: %q", line)
		}
	}
}
