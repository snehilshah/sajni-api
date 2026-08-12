package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	// Embed the IANA tz database so time.LoadLocation works on the
	// distroless Cloud Run image (no system zoneinfo). Needed to render
	// reminder emails in the user's local clock time.
	"sajni/internal/ai"
	"sajni/internal/api"
	"sajni/internal/auth"
	"sajni/internal/config"
	"sajni/internal/db"
	"sajni/internal/logger"
	"sajni/internal/push"
	"sajni/internal/reminderqueue"
	"sajni/internal/storage"
	_ "time/tzdata"

	"github.com/rs/zerolog/log"
)

func main() {
	ctx := context.Background()

	port := flag.Int("port", 8080, "HTTP server port")
	flag.Parse()

	cfg, err := config.Load(".env")
	if err != nil {
		fmt.Fprintf(os.Stderr, "configuration: %v\n", err)
		os.Exit(1)
	}
	if err := logger.Init(cfg.Environment, cfg.Logging.Level); err != nil {
		fmt.Fprintf(os.Stderr, "configure logger: %v\n", err)
		os.Exit(1)
	}

	database, err := db.New(cfg.Database.URL, cfg.Database.DropAndReseed)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize database")
	}
	defer database.Close()

	store, err := storage.New(ctx, cfg.Storage)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize storage")
	}

	authSvc, err := auth.NewService(database, cfg.Auth)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize auth")
	}

	reminderQueue := reminderqueue.New(cfg.Reminders, cfg.Auth.APIBaseURL)
	aiSvc, err := ai.NewService(ctx, database, store, cfg.AI, cfg.Media, reminderQueue)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize AI service")
	}

	pushSvc, err := push.New(ctx, cfg.Push)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize push sender")
	}

	// Log startup state once — not on each request.
	evt := log.Info().
		Str("environment", string(cfg.Environment)).
		Int("port", *port).
		Str("storage", cfg.Storage.Backend)
	if aiSvc != nil {
		evt = evt.Str("model", aiSvc.Model())
	} else {
		evt = evt.Bool("ai_enabled", false)
	}
	cloudTasksEnabled := cfg.Reminders.CloudTasksProject != "" &&
		cfg.Reminders.CloudTasksQueue != "" &&
		cfg.Reminders.CronSecret != ""
	evt = evt.
		Bool("tmdb", cfg.Media.TMDBAPIKey != "").
		Bool("places", cfg.Media.GooglePlacesAPIKey != "").
		Bool("cloud_tasks", cloudTasksEnabled).
		Bool("push", pushSvc != nil)
	evt.Msg("sajni started")

	deps := api.Deps{
		Environment:   cfg.Environment,
		HTTP:          cfg.HTTP,
		Media:         cfg.Media,
		Reminders:     cfg.Reminders,
		ReminderQueue: reminderQueue,
		DB:            database,
		Auth:          authSvc,
		Storage:       store,
		AI:            aiSvc,
		Push:          pushSvc,
	}
	handler := api.Router(deps)

	// Background ticks. All work below is idempotent so missed ticks during
	// deploys are harmless; the next tick catches up.
	//
	//   hourly  — purge soft-deleted users past the 7d window
	//           — process billers (post auto-renew txns, raise upcoming alerts)
	//           — process investment auto-debits (post contribution txns)
	//   daily   — generate per-window insights (1w / 2w / 1m / 6m / 1y)
	go func() {
		// Run a pass at boot so a long downtime doesn't leave the queue stale.
		if n, err := api.PurgeExpiredDeletedUsers(context.Background(), deps); err == nil && n > 0 {
			log.Info().Int64("users_purged", n).Msg("expired accounts purged at boot")
		}
		if posted, alerts, err := api.ProcessBillerCron(context.Background(), deps); err == nil && (posted+alerts) > 0 {
			log.Info().Int("auto_posted", posted).Int("upcoming", alerts).Msg("billers processed at boot")
		}
		if posted, err := api.ProcessInvestmentDebits(context.Background(), deps); err == nil && posted > 0 {
			log.Info().Int("debits", posted).Msg("investment auto-debits posted at boot")
		}
		if reminded, graduated, err := api.ProcessMediaReleaseCron(context.Background(), deps); err != nil {
			log.Warn().Err(err).Msg("media release processing failed at boot")
		} else if reminded+graduated > 0 {
			log.Info().Int("reminded", reminded).Int("graduated", graduated).Msg("media releases processed at boot")
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
				if posted, err := api.ProcessInvestmentDebits(context.Background(), deps); err != nil {
					log.Warn().Err(err).Msg("investment debit cron failed")
				} else if posted > 0 {
					log.Info().Int("debits", posted).Msg("investment auto-debits posted")
				}
				if reminded, graduated, err := api.ProcessMediaReleaseCron(context.Background(), deps); err != nil {
					log.Warn().Err(err).Msg("media release processing failed")
				} else if reminded+graduated > 0 {
					log.Info().Int("reminded", reminded).Int("graduated", graduated).Msg("media releases processed")
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
