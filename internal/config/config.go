package config

import (
	"fmt"
	"os"
	"strings"
)

type Environment string

const (
	Local      Environment = "local"
	Production Environment = "production"
)

type Database struct {
	URL           string
	DropAndReseed bool
}

type Logging struct {
	Level string
}

type HTTP struct {
	CORSOrigin     string
	AllowLocalCORS bool
}

type Storage struct {
	Backend   string
	LocalDir  string
	GCSBucket string
}

type Auth struct {
	JWTSecret          string
	CookieInsecure     bool
	AppURL             string
	APIBaseURL         string
	GoogleClientID     string
	GoogleClientSecret string
	GitHubClientID     string
	GitHubClientSecret string
	ResendAPIKey       string
	EmailFrom          string
	DevLogEmailCodes   bool
	DevAuthBypass      bool
	DevAuthBypassEmail string
	DevAuthBypassName  string
}

type AI struct {
	GeminiAPIKey string
	GeminiModel  string
}

type Media struct {
	TMDBAPIKey         string
	GooglePlacesAPIKey string
}

type Reminders struct {
	CloudTasksProject  string
	CloudTasksLocation string
	CloudTasksQueue    string
	CronSecret         string
	InsightCronSecret  string
}

type Push struct {
	FirebaseProjectID string
}

type Config struct {
	Environment Environment
	Database    Database
	Logging     Logging
	HTTP        HTTP
	Storage     Storage
	Auth        Auth
	AI          AI
	Media       Media
	Reminders   Reminders
	Push        Push
}

func Load(dotEnvPath string) (Config, error) {
	if err := LoadDotEnv(dotEnvPath); err != nil {
		return Config{}, err
	}

	database, err := readDatabase()
	if err != nil {
		return Config{}, err
	}
	cookieInsecure, err := readBool("COOKIE_INSECURE")
	if err != nil {
		return Config{}, err
	}
	allowLocalCORS, err := readBool("ALLOW_LOCAL_CORS")
	if err != nil {
		return Config{}, err
	}
	devLogEmailCodes, err := readBool("AUTH_DEV_CODE_LOG")
	if err != nil {
		return Config{}, err
	}
	devAuthBypass, err := readBool("DEV_AUTH_BYPASS")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Environment: Environment(strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))),
		Database:    database,
		Logging:     Logging{Level: strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))},
		HTTP: HTTP{
			CORSOrigin:     strings.TrimRight(strings.TrimSpace(os.Getenv("CORS_ORIGIN")), "/"),
			AllowLocalCORS: allowLocalCORS,
		},
		Storage: Storage{
			Backend:   strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_BACKEND"))),
			LocalDir:  strings.TrimSpace(os.Getenv("STORAGE_LOCAL_DIR")),
			GCSBucket: strings.TrimSpace(os.Getenv("GCS_BUCKET")),
		},
		Auth: Auth{
			JWTSecret:          os.Getenv("JWT_SECRET"),
			CookieInsecure:     cookieInsecure,
			AppURL:             strings.TrimRight(strings.TrimSpace(os.Getenv("APP_URL")), "/"),
			APIBaseURL:         strings.TrimRight(strings.TrimSpace(os.Getenv("API_BASE_URL")), "/"),
			GoogleClientID:     strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CLIENT_ID")),
			GoogleClientSecret: os.Getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
			GitHubClientID:     strings.TrimSpace(os.Getenv("GITHUB_OAUTH_CLIENT_ID")),
			GitHubClientSecret: os.Getenv("GITHUB_OAUTH_CLIENT_SECRET"),
			ResendAPIKey:       os.Getenv("RESEND_API_KEY"),
			EmailFrom:          strings.TrimSpace(os.Getenv("EMAIL_FROM")),
			DevLogEmailCodes:   devLogEmailCodes,
			DevAuthBypass:      devAuthBypass,
			DevAuthBypassEmail: strings.ToLower(strings.TrimSpace(os.Getenv("DEV_AUTH_BYPASS_EMAIL"))),
			DevAuthBypassName:  strings.TrimSpace(os.Getenv("DEV_AUTH_BYPASS_NAME")),
		},
		AI: AI{
			GeminiAPIKey: os.Getenv("GEMINI_API_KEY"),
			GeminiModel:  "gemini-3.7-flash",
		},
		Media: Media{
			TMDBAPIKey:         os.Getenv("TMDB_API_KEY"),
			GooglePlacesAPIKey: os.Getenv("GOOGLE_PLACES_KEY"),
		},
		Reminders: Reminders{
			CloudTasksProject:  strings.TrimSpace(os.Getenv("CLOUD_TASKS_PROJECT")),
			CloudTasksLocation: strings.TrimSpace(os.Getenv("CLOUD_TASKS_LOCATION")),
			CloudTasksQueue:    strings.TrimSpace(os.Getenv("CLOUD_TASKS_QUEUE")),
			CronSecret:         os.Getenv("REMINDER_CRON_SECRET"),
			InsightCronSecret:  os.Getenv("INSIGHT_CRON_SECRET"),
		},
		Push: Push{FirebaseProjectID: strings.TrimSpace(os.Getenv("FIREBASE_PROJECT_ID"))},
	}

	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func LoadDatabase(dotEnvPath string) (Database, error) {
	if err := LoadDotEnv(dotEnvPath); err != nil {
		return Database{}, err
	}
	database, err := readDatabase()
	if err != nil {
		return Database{}, err
	}
	if database.URL == "" {
		return Database{}, fmt.Errorf("DATABASE_URL is required")
	}
	return database, nil
}

func readDatabase() (Database, error) {
	dropAndReseed, err := readBool("DROP_AND_RESEED")
	if err != nil {
		return Database{}, err
	}
	database := Database{
		URL:           strings.TrimSpace(os.Getenv("DATABASE_URL")),
		DropAndReseed: dropAndReseed,
	}
	return database, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = "info"
	}
	if cfg.Storage.Backend == "" {
		cfg.Storage.Backend = "local"
	}
	if cfg.Storage.LocalDir == "" {
		cfg.Storage.LocalDir = "./data/blobs"
	}
	if cfg.Auth.AppURL == "" && cfg.Environment == Local {
		cfg.Auth.AppURL = "http://localhost:5173"
	}
	if cfg.Auth.APIBaseURL == "" && cfg.Environment == Local {
		cfg.Auth.APIBaseURL = "http://localhost:8080"
	}
	if cfg.Auth.DevAuthBypassEmail == "" {
		cfg.Auth.DevAuthBypassEmail = "dev@sajni.local"
	}
	if cfg.Auth.DevAuthBypassName == "" {
		cfg.Auth.DevAuthBypassName = "Sajni Dev"
	}
	if cfg.Reminders.CloudTasksLocation == "" {
		cfg.Reminders.CloudTasksLocation = "asia-south1"
	}
}

func validate(cfg Config) error {
	var problems []string

	if cfg.Environment != Local && cfg.Environment != Production {
		problems = append(problems, `APP_ENV must be "local" or "production"`)
	}
	if cfg.Database.URL == "" {
		problems = append(problems, "DATABASE_URL is required")
	}
	if cfg.Auth.JWTSecret == "" {
		problems = append(problems, "JWT_SECRET is required")
	}
	if cfg.Auth.ResendAPIKey == "" && !cfg.Auth.DevLogEmailCodes {
		problems = append(problems, "RESEND_API_KEY is required unless AUTH_DEV_CODE_LOG=1")
	}
	if cfg.Auth.ResendAPIKey != "" && cfg.Auth.EmailFrom == "" {
		problems = append(problems, "EMAIL_FROM is required when RESEND_API_KEY is configured")
	}

	switch cfg.Storage.Backend {
	case "local":
	case "gcs":
		if cfg.Storage.GCSBucket == "" {
			problems = append(problems, "GCS_BUCKET is required when STORAGE_BACKEND=gcs")
		}
	default:
		problems = append(problems, `STORAGE_BACKEND must be "local" or "gcs"`)
	}

	if (cfg.Auth.GoogleClientID == "") != (cfg.Auth.GoogleClientSecret == "") {
		problems = append(problems, "GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET must be configured together")
	}
	if (cfg.Auth.GitHubClientID == "") != (cfg.Auth.GitHubClientSecret == "") {
		problems = append(problems, "GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET must be configured together")
	}

	cloudTasksConfigured := cfg.Reminders.CloudTasksProject != "" || cfg.Reminders.CloudTasksQueue != "" || cfg.Reminders.CronSecret != ""
	if cloudTasksConfigured {
		if cfg.Reminders.CloudTasksProject == "" {
			problems = append(problems, "CLOUD_TASKS_PROJECT is required when Cloud Tasks reminders are configured")
		}
		if cfg.Reminders.CloudTasksQueue == "" {
			problems = append(problems, "CLOUD_TASKS_QUEUE is required when Cloud Tasks reminders are configured")
		}
		if cfg.Auth.APIBaseURL == "" {
			problems = append(problems, "API_BASE_URL is required when Cloud Tasks reminders are configured")
		}
		if cfg.Reminders.CronSecret == "" {
			problems = append(problems, "REMINDER_CRON_SECRET is required when Cloud Tasks reminders are configured")
		}
	}

	switch cfg.Logging.Level {
	case "trace", "debug", "info", "warn", "error", "fatal", "panic", "disabled":
	default:
		problems = append(problems, "LOG_LEVEL is invalid")
	}

	if cfg.Environment == Production {
		if cfg.Storage.Backend != "gcs" {
			problems = append(problems, "STORAGE_BACKEND must be gcs in production")
		}
		if cfg.Auth.AppURL == "" {
			problems = append(problems, "APP_URL is required in production")
		}
		if cfg.Auth.APIBaseURL == "" {
			problems = append(problems, "API_BASE_URL is required in production")
		}
		if cfg.HTTP.CORSOrigin == "" {
			problems = append(problems, "CORS_ORIGIN is required in production")
		}
		if cfg.Auth.CookieInsecure {
			problems = append(problems, "COOKIE_INSECURE cannot be enabled in production")
		}
		if cfg.HTTP.AllowLocalCORS {
			problems = append(problems, "ALLOW_LOCAL_CORS cannot be enabled in production")
		}
		if cfg.Auth.DevLogEmailCodes {
			problems = append(problems, "AUTH_DEV_CODE_LOG cannot be enabled in production")
		}
		if cfg.Auth.DevAuthBypass {
			problems = append(problems, "DEV_AUTH_BYPASS cannot be enabled in production")
		}
		if cfg.Database.DropAndReseed {
			problems = append(problems, "DROP_AND_RESEED cannot be enabled in production")
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n- %s", strings.Join(problems, "\n- "))
}

func readBool(name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	switch value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s must be 0 or 1", name)
	}
}
