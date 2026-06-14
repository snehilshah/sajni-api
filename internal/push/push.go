// Package push delivers FCM notifications to registered native devices.
//
// It speaks the FCM HTTP v1 API directly with an ADC token source instead of
// pulling the firebase-admin SDK: on Cloud Run the runtime service account
// (granted roles/firebasemessaging.admin) is the credential, locally it's
// whatever `gcloud auth application-default login` set up. Disabled (nil
// sender) when FIREBASE_PROJECT_ID is unset, so dev environments without
// Firebase keep working on the email-only path.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"sajni/internal/db"
)

const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// Sender ships notification messages to FCM. Zero-value is not usable; use New.
type Sender struct {
	projectID string
	tokens    oauth2.TokenSource
	client    *http.Client
}

// New returns a ready Sender, or (nil, nil) when FIREBASE_PROJECT_ID is unset
// (push disabled). An ADC failure is a real error: the env var says push
// should work, so a missing credential must surface at boot.
func New(ctx context.Context) (*Sender, error) {
	projectID := os.Getenv("FIREBASE_PROJECT_ID")
	if projectID == "" {
		return nil, nil
	}
	ts, err := google.DefaultTokenSource(ctx, fcmScope)
	if err != nil {
		return nil, fmt.Errorf("push: application default credentials: %w", err)
	}
	return &Sender{
		projectID: projectID,
		tokens:    oauth2.ReuseTokenSource(nil, ts),
		client:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Notification is one user-facing push. Route is the in-app path the tap
// should open ("/tasks", "/finance"), carried as FCM data so the client can
// deep-link.
type Notification struct {
	Title string
	Body  string
	Route string
}

// SendToUser delivers n to every device the user has registered and returns
// how many sends succeeded. Tokens FCM reports as gone are pruned. A zero
// return with nil error means "no live device"; callers email in addition
// to push either way.
func (s *Sender) SendToUser(ctx context.Context, d *db.DB, uid string, n Notification) (int, error) {
	rows, err := d.QueryContext(ctx, `SELECT token FROM push_devices WHERE user_id = $1`, uid)
	if err != nil {
		return 0, err
	}
	var tokens []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			tokens = append(tokens, t)
		}
	}
	rows.Close()

	sent := 0
	for _, token := range tokens {
		switch err := s.send(ctx, token, n); {
		case err == nil:
			sent++
		case isGoneToken(err):
			d.ExecContext(ctx, `DELETE FROM push_devices WHERE token = $1`, token)
			log.Info().Str("user", uid).Msg("pruned dead push token")
		default:
			log.Warn().Err(err).Str("user", uid).Msg("push send failed")
		}
	}
	return sent, nil
}

// errGone marks a token FCM no longer recognizes (uninstalled / rotated).
type errGone struct{ msg string }

func (e errGone) Error() string { return e.msg }

func isGoneToken(err error) bool {
	_, ok := err.(errGone)
	return ok
}

// send posts one message to the FCM v1 endpoint.
func (s *Sender) send(ctx context.Context, token string, n Notification) error {
	payload := map[string]any{
		"message": map[string]any{
			"token": token,
			"notification": map[string]string{
				"title": n.Title,
				"body":  n.Body,
			},
			"data": map[string]string{
				"route": n.Route,
			},
			"android": map[string]any{
				"priority": "high",
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	tok, err := s.tokens.Token()
	if err != nil {
		return fmt.Errorf("push: token source: %w", err)
	}

	url := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", s.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	tok.SetAuthHeader(req)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	// 404 NOT_FOUND / UNREGISTERED = token is gone; everything else is a
	// transient or config error worth logging upstream.
	if resp.StatusCode == http.StatusNotFound || strings.Contains(string(respBody), "UNREGISTERED") {
		return errGone{msg: fmt.Sprintf("fcm token gone (%d)", resp.StatusCode)}
	}
	return fmt.Errorf("fcm send: %d: %s", resp.StatusCode, respBody)
}
