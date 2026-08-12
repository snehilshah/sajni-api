package logger

import (
	"os"
	"time"

	"sajni/internal/config"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Init configures the global zerolog logger.
//
// Production: JSON to stdout with Cloud Logging severity mapping.
// Local: pretty console output.
// Level is validated by config.Load before initialization.
func Init(environment config.Environment, levelName string) error {
	if environment == config.Production {
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

	level, err := zerolog.ParseLevel(levelName)
	if err != nil {
		return err
	}
	zerolog.SetGlobalLevel(level)
	return nil
}
