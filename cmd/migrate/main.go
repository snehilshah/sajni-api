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
	"fmt"
	"os"

	_ "time/tzdata"

	"github.com/rs/zerolog/log"

	"sajni/internal/config"
	"sajni/internal/db"
)

func main() {
	databaseConfig, err := config.LoadDatabase(".env")
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration: %v\n", err)
		os.Exit(1)
	}
	database, err := db.New(databaseConfig.URL, databaseConfig.DropAndReseed)
	if err != nil {
		log.Fatal().Err(err).Msg("migrate failed")
	}
	defer database.Close()
	log.Info().Msg("migrations applied")
}
