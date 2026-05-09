package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

// Register attaches the auth routes to the provided mux. These routes
// must NOT be wrapped by the auth middleware.
func (s *Service) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	// /me sits behind the protected zone; the router wires it there.
}

type authBody struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
}

type authResponse struct {
	AccessToken string       `json:"access_token"`
	User        userResponse `json:"user"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func validateCreds(b authBody) error {
	email := strings.TrimSpace(strings.ToLower(b.Email))
	if email == "" || !strings.Contains(email, "@") {
		return errors.New("valid email required")
	}
	if len(b.Password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

func (s *Service) handleRegister(w http.ResponseWriter, r *http.Request) {
	var body authBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if err := validateCreds(body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))

	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "hash failure")
		return
	}

	var id int64
	err = s.DB.QueryRow(
		"INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id",
		email, string(hash),
	).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeErr(w, http.StatusConflict, "email already registered")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	access, err := s.issueAccessToken(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue access token")
		return
	}
	refresh, err := s.issueRefreshToken(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue refresh token")
		return
	}
	s.setRefreshCookie(w, refresh)
	writeJSON(w, http.StatusCreated, authResponse{
		AccessToken: access,
		User:        userResponse{ID: id, Email: email},
	})
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body authBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))

	var (
		id   int64
		hash string
	)
	err := s.DB.QueryRow(
		"SELECT id, password_hash FROM users WHERE email = $1",
		email,
	).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(body.Password)) != nil {
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}

	access, err := s.issueAccessToken(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue access token")
		return
	}
	refresh, err := s.issueRefreshToken(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue refresh token")
		return
	}
	s.setRefreshCookie(w, refresh)
	writeJSON(w, http.StatusOK, authResponse{
		AccessToken: access,
		User:        userResponse{ID: id, Email: email},
	})
}

func (s *Service) handleRefresh(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookie)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "no refresh token")
		return
	}
	userID, err := s.consumeRefreshToken(cookie.Value)
	if err != nil {
		s.clearRefreshCookie(w)
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	}

	// Look up email to round-trip user info.
	var email string
	if err := s.DB.QueryRow("SELECT email FROM users WHERE id = $1", userID).Scan(&email); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	access, err := s.issueAccessToken(userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue access token")
		return
	}
	refresh, err := s.issueRefreshToken(userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "issue refresh token")
		return
	}
	s.setRefreshCookie(w, refresh)
	writeJSON(w, http.StatusOK, authResponse{
		AccessToken: access,
		User:        userResponse{ID: userID, Email: email},
	})
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(refreshCookie)
	if err == nil && cookie.Value != "" {
		// Best-effort revoke; if it fails to match we still clear the cookie.
		_, _ = s.consumeRefreshToken(cookie.Value)
	}
	s.clearRefreshCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleMe is a protected handler that returns the current user.
func (s *Service) HandleMe(w http.ResponseWriter, r *http.Request) {
	id, ok := UserIDFromContext(r.Context())
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var email string
	if err := s.DB.QueryRow("SELECT email FROM users WHERE id = $1", id).Scan(&email); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, userResponse{ID: id, Email: email})
}
