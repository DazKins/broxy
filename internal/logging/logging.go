package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

const EnvLogLevel = "BROXY_LOG_LEVEL"

func FromEnv() *slog.Logger {
	return New(os.Getenv(EnvLogLevel), os.Stderr)
}

func New(level string, writer io.Writer) *slog.Logger {
	if writer == nil {
		writer = io.Discard
	}
	return slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{
		Level: parseLevel(level),
	}))
}

func parseLevel(value string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
