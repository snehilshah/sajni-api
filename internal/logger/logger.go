package logger

import (
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Init configures the global zerolog logger.
//
// Cloud Run (K_SERVICE set): JSON to stdout with Cloud Logging severity mapping.
// Local: pretty console output.
// Level: info by default; override with LOG_LEVEL=debug|warn|error.
func Init() {
	if os.Getenv("K_SERVICE") != "" {
		// Cloud Logging parses "severity" field for log levels.
		zerolog.LevelFieldName = "severity"
		zerolog.LevelDebugValue = "DEBUG"
		zerolog.LevelInfoValue = "INFO"
		zerolog.LevelWarnValue = "WARNING"
		zerolog.LevelErrorValue = "ERROR"
		zerolog.LevelFatalValue = "CRITICAL"
		zerolog.LevelPanicValue = "CRITICAL"
		zerolog.TimeFieldFormat = time.RFC3339Nano
		log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	} else {
		log.Logger = zerolog.New(zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: "15:04:05",
		}).With().Timestamp().Logger()
	}

	level := zerolog.InfoLevel
	if l, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL")); err == nil {
		level = l
	}
	zerolog.SetGlobalLevel(level)
}
