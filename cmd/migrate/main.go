// Command migrate applies the schema migrations (db.New runs migrate()) against
// DATABASE_URL and exits — no HTTP server, no background ticks. Useful for
// applying schema/backfill changes to a live DB out-of-band from a deploy.
//
//	go run ./cmd/migrate      # reads DATABASE_URL from .env, like the server
//
// All migrations are idempotent (CREATE ... IF NOT EXISTS, idempotent backfill
// UPDATEs), so running this repeatedly is safe.
package main

import (
	"bufio"
	"os"
	"strings"

	_ "time/tzdata"

	"github.com/rs/zerolog/log"

	"sajni/internal/db"
	"sajni/internal/logger"
)

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), `"'`)
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
}

func main() {
	loadDotEnv(".env")
	logger.Init()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal().Msg("DATABASE_URL is required")
	}
	database, err := db.New(dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("migrate failed")
	}
	defer database.Close()
	log.Info().Msg("migrations applied")
}
