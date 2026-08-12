package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig(environment Environment) Config {
	cfg := Config{
		Environment: environment,
		Database:    Database{URL: "postgres://local"},
		Logging:     Logging{Level: "info"},
		HTTP:        HTTP{},
		Storage:     Storage{Backend: "local", LocalDir: "./data/blobs"},
		Auth: Auth{
			JWTSecret:        "test-secret",
			AppURL:           "http://localhost:5173",
			APIBaseURL:       "http://localhost:8080",
			DevLogEmailCodes: true,
		},
		AI:        AI{GeminiModel: "gemini-test"},
		Reminders: Reminders{CloudTasksLocation: "asia-south1"},
	}
	if environment == Production {
		cfg.HTTP.CORSOrigin = "https://www.ohmysajni.com"
		cfg.Storage = Storage{Backend: "gcs", GCSBucket: "sajni-blobs"}
		cfg.Auth.AppURL = "https://www.ohmysajni.com"
		cfg.Auth.APIBaseURL = "https://api.ohmysajni.com"
		cfg.Auth.ResendAPIKey = "resend-key"
		cfg.Auth.EmailFrom = "Sajni <hello@ohmysajni.com>"
		cfg.Auth.DevLogEmailCodes = false
	}
	return cfg
}

func TestValidateEnvironments(t *testing.T) {
	for _, environment := range []Environment{Local, Production} {
		t.Run(string(environment), func(t *testing.T) {
			if err := validate(validConfig(environment)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateProductionRejectsLocalFlags(t *testing.T) {
	cfg := validConfig(Production)
	cfg.Auth.CookieInsecure = true
	cfg.Auth.DevAuthBypass = true
	cfg.Database.DropAndReseed = true

	err := validate(cfg)
	if err == nil {
		t.Fatal("expected production validation error")
	}
	for _, name := range []string{"COOKIE_INSECURE", "DEV_AUTH_BYPASS", "DROP_AND_RESEED"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("validation error does not mention %s: %v", name, err)
		}
	}
}

func TestValidateRequiresCompleteOAuthPair(t *testing.T) {
	cfg := validConfig(Local)
	cfg.Auth.GoogleClientID = "client-id"

	err := validate(cfg)
	if err == nil || !strings.Contains(err.Error(), "GOOGLE_OAUTH_CLIENT_SECRET") {
		t.Fatalf("expected incomplete Google OAuth error, got %v", err)
	}
}

func TestLoadDotEnvPreservesExistingEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("APP_ENV=local\nDATABASE_URL=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", "from-process")

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("DATABASE_URL"); got != "from-process" {
		t.Fatalf("DATABASE_URL = %q, want process value", got)
	}
}

func TestLoadDotEnvReportsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("APP_ENV local\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := LoadDotEnv(path)
	if err == nil || !strings.Contains(err.Error(), ":1") {
		t.Fatalf("expected line-numbered parse error, got %v", err)
	}
}

func TestLoadDotEnvReportsScannerFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	longValue := strings.Repeat("x", 70*1024)
	if err := os.WriteFile(path, []byte("TOO_LONG="+longValue+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := LoadDotEnv(path)
	if err == nil || !strings.Contains(err.Error(), "read dotenv") {
		t.Fatalf("expected scanner error, got %v", err)
	}
}
