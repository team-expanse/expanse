package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// Options configures the logger.
type Options struct {
	Level  string    // debug|info|warn|error
	Format string    // text|json
	Output io.Writer // defaults to os.Stdout
}

func Setup(o Options) (*slog.Logger, error) {
	var level slog.Level
	switch o.Level {
	case "debug":
		level = slog.LevelDebug
	case "info":
		fallthrough
	case "":
		level = slog.LevelInfo
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level: %s", o.Level)
	}

	var handler slog.Handler
	if o.Output == nil {
		o.Output = os.Stdout
	}
	switch o.Format {
	case "json":
		handler = slog.NewJSONHandler(o.Output, &slog.HandlerOptions{Level: level})
	default:
		handler = slog.NewTextHandler(o.Output, &slog.HandlerOptions{Level: level})
	}

	return slog.New(handler), nil
}

// Default returns a logger with info-level text output to stdout.
func Default() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// WithComponent adds a "component" attribute to the logger.
func WithComponent(logger *slog.Logger, name string) *slog.Logger {
	return logger.With("component", name)
}
