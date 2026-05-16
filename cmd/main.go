package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"

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

	handler := api.Router(api.Deps{
		DB:      database,
		Auth:    authSvc,
		Storage: store,
		AI:      aiSvc,
	}, *frontendDir)

	addr := fmt.Sprintf(":%d", *port)
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatal().Err(err).Msg("server error")
	}
}
