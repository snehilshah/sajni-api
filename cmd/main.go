package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	// Embed the IANA tz database so time.LoadLocation works on the
	// distroless Cloud Run image (no system zoneinfo). Needed to render
	// reminder emails in the user's local clock time.
	_ "time/tzdata"

	"github.com/rs/zerolog/log"

	"sajni/internal/ai"
	"sajni/internal/api"
	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/logger"
	"sajni/internal/storage"
)

// loadDotEnv reads KEY=VALUE lines from path and sets them as env vars
// (only if not already present). Missing file is not an error.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
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

	port := flag.Int("port", 8080, "HTTP server port")
	frontendDir := flag.String("frontend", "", "Path to built frontend directory (optional)")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal().Msg("DATABASE_URL is required")
	}

	database, err := db.New(dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize database")
	}
	defer database.Close()

	ctx := context.Background()
	store, err := storage.New(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize storage")
	}

	authSvc, err := auth.NewService(database)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize auth")
	}

	aiSvc, err := ai.NewService(ctx, database, store)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize AI service")
	}

	// Log startup state once — not on each request.
	backend := os.Getenv("STORAGE_BACKEND")
	if backend == "" {
		backend = "local"
	}
	evt := log.Info().Int("port", *port).Str("storage", backend)
	if aiSvc != nil {
		evt = evt.Str("model", aiSvc.Model())
	} else {
		evt = evt.Bool("ai_enabled", false)
	}
	if os.Getenv("TMDB_API_KEY") == "" {
		evt = evt.Bool("tmdb", false)
	}
	evt.Msg("sajni started")

	deps := api.Deps{
		DB:      database,
		Auth:    authSvc,
		Storage: store,
		AI:      aiSvc,
	}
	handler := api.Router(deps, *frontendDir)

	// Background ticks. All work below is idempotent so missed ticks during
	// deploys are harmless; the next tick catches up.
	//
	//   hourly  — purge soft-deleted users past the 7d window
	//           — process billers (post auto-renew txns, raise upcoming alerts)
	//   daily   — generate per-window insights (1w / 2w / 1m / 6m / 1y)
	go func() {
		// Run a pass at boot so a long downtime doesn't leave the queue stale.
		if n, err := api.PurgeExpiredDeletedUsers(context.Background(), deps); err == nil && n > 0 {
			log.Info().Int64("users_purged", n).Msg("expired accounts purged at boot")
		}
		if posted, alerts, err := api.ProcessBillerCron(context.Background(), deps); err == nil && (posted+alerts) > 0 {
			log.Info().Int("auto_posted", posted).Int("upcoming", alerts).Msg("billers processed at boot")
		}

		hourly := time.NewTicker(time.Hour)
		daily := time.NewTicker(24 * time.Hour)
		defer hourly.Stop()
		defer daily.Stop()
		for {
			select {
			case <-hourly.C:
				if n, err := api.PurgeExpiredDeletedUsers(context.Background(), deps); err != nil {
					log.Warn().Err(err).Msg("purge expired accounts failed")
				} else if n > 0 {
					log.Info().Int64("users_purged", n).Msg("expired accounts purged")
				}
				if posted, alerts, err := api.ProcessBillerCron(context.Background(), deps); err != nil {
					log.Warn().Err(err).Msg("biller cron failed")
				} else if posted+alerts > 0 {
					log.Info().Int("auto_posted", posted).Int("upcoming", alerts).Msg("billers processed")
				}
			case <-daily.C:
				if n, err := api.RunDailyInsightCron(context.Background(), deps); err != nil {
					log.Warn().Err(err).Msg("insight cron failed")
				} else if n > 0 {
					log.Info().Int("insights", n).Msg("insights generated")
				}
			}
		}
	}()

	addr := fmt.Sprintf(":%d", *port)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal().Err(err).Msg("server error")
	}
}
