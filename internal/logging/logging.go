// Package logging builds the process-wide structured logger.
//
// free-router used to rely on slog's implicit default: text format at info
// level on stderr, with no way to ask for more. That is the wrong default for
// a gateway whose most common failure mode is "the pool fell back three times
// and nobody knows which provider answered". Operators need a switch for
// debug detail exactly when they are chasing a fallback, and a JSON format
// when the log is going to be piped into something that is not a human.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Formats accepted by ParseFormat.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// Options describes how the process logger should be built. Zero values mean
// "the shipped default": info level, text format, stderr.
type Options struct {
	Level  string
	Format string
	Writer io.Writer
}

// New builds a logger from opts. Unknown levels and formats are rejected
// rather than silently downgraded: a typo in FREE_ROUTER_LOG_LEVEL would
// otherwise look exactly like "logging is broken".
func New(opts Options) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	writer := opts.Writer
	if writer == nil {
		writer = os.Stderr
	}
	handlerOptions := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch strings.ToLower(strings.TrimSpace(opts.Format)) {
	case "", FormatText:
		handler = slog.NewTextHandler(writer, handlerOptions)
	case FormatJSON:
		handler = slog.NewJSONHandler(writer, handlerOptions)
	default:
		return nil, fmt.Errorf("unknown log format %q (want text or json)", opts.Format)
	}
	return slog.New(handler), nil
}

// ParseLevel maps a textual level to slog.Level. An empty value means info,
// matching slog's own default so an unconfigured deployment keeps its old
// verbosity.
func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", value)
	}
}

// Setup installs the logger described by opts as the process default and
// returns it. Every package logs through slog's default, so one call here
// governs the gateway, the catalog and the admin background jobs alike.
func Setup(opts Options) (*slog.Logger, error) {
	logger, err := New(opts)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(logger)
	return logger, nil
}
