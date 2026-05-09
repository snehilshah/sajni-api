package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"sajni/internal/ai"
	"sajni/internal/api"
	"sajni/internal/auth"
	"sajni/internal/db"
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

	port := flag.Int("port", 8080, "HTTP server port")
	frontendDir := flag.String("frontend", "", "Path to built frontend directory (optional)")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (e.g. postgres://user:pass@localhost:5432/sajni?sslmode=disable)")
	}

	database, err := db.New(dsn)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	store, err := storage.New(ctx)
	if err != nil {
		log.Fatalf("Failed to initialize storage: %v", err)
	}

	authSvc, err := auth.NewService(database)
	if err != nil {
		log.Fatalf("Failed to initialize auth: %v", err)
	}

	aiSvc, err := ai.NewService(ctx, database, store)
	if err != nil {
		log.Fatalf("Failed to initialize AI service: %v", err)
	}
	if aiSvc == nil {
		log.Printf("Note: GEMINI_API_KEY not set — AI features disabled")
	} else {
		log.Printf("AI enabled (model: %s)", aiSvc.Model())
	}

	handler := api.Router(api.Deps{
		DB:      database,
		Auth:    authSvc,
		Storage: store,
		AI:      aiSvc,
	}, *frontendDir)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Sajni is running at http://localhost%s", addr)
	if backend := os.Getenv("STORAGE_BACKEND"); backend != "" {
		log.Printf("Storage backend: %s", backend)
	} else {
		log.Printf("Storage backend: local")
	}
	if os.Getenv("TMDB_API_KEY") == "" {
		log.Printf("Note: TMDB_API_KEY not set — movie/show search will be empty")
	}
	if err := http.ListenAndServe(addr, handler); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
