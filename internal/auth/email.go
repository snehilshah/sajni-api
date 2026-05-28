package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

//go:embed email_templates/*.html
var emailTemplatesFS embed.FS

const (
	emailCodeTTL      = 10 * time.Minute
	emailCodeAttempts = 5
)

// generateNumericCode returns a 6-digit zero-padded one-time code.
// crypto/rand source so codes aren't predictable.
func generateNumericCode() (string, error) {
	max := big.NewInt(1_000_000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func hashCode(code string) []byte {
	h := sha256.Sum256([]byte(code))
	return h[:]
}

// sendEmailCode issues a fresh TOTP, persists its hash, and ships the
// HTML via Resend. purpose is "login" for first-class sign-in OR
// "link" when we're challenging the user to prove ownership of an
// existing account before attaching a new identity to it.
func (s *Service) sendEmailCode(ctx context.Context, email, name, purpose string, linkUserID, linkProvider, linkSubject, linkName string) error {
	// Soft rate-limit: max 3 sends per email per 60 minutes.
	var recent int
	s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM email_codes WHERE email=$1 AND created_at > NOW() - INTERVAL '1 hour'`,
		email).Scan(&recent)
	if recent >= 3 {
		return errors.New("too many code requests; try again later")
	}

	code, err := generateNumericCode()
	if err != nil {
		return err
	}

	// pgx needs an explicit ::uuid cast on the NULLIF expression because
	// link_user_id is a UUID column and Postgres can't infer the type
	// when both sides of the NULLIF are untyped text — that's the
	// `could not determine data type of parameter $5 (SQLSTATE 42P18)`.
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO email_codes (id, email, code_hash, purpose, link_user_id, link_provider, link_subject, link_name, expires_at)
		 VALUES ($1, $2, $3, $4, NULLIF($5,'')::uuid, NULLIF($6,''), NULLIF($7,''), NULLIF($8,''), $9)`,
		NewID(), email, hashCode(code), purpose,
		linkUserID, linkProvider, linkSubject, linkName,
		time.Now().Add(emailCodeTTL),
	); err != nil {
		return err
	}

	return s.deliverCodeEmail(ctx, email, name, code, purpose)
}

// renderTOTPEmail loads the embedded template and fills in code + copy.
func (s *Service) renderTOTPEmail(email, name, code, purpose string) (string, string, error) {
	tpl, err := template.ParseFS(emailTemplatesFS, "email_templates/totp.html")
	if err != nil {
		return "", "", err
	}
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		displayName = strings.SplitN(email, "@", 2)[0]
	}
	greeting := "Your sign-in code"
	intro := "Enter this code in Sajni to sign in. It's good for one use only."
	subject := "Your Sajni sign-in code"
	if purpose == "link" {
		greeting = "Confirm linking your account"
		intro = "You're trying to add a new sign-in method to your Sajni account. Enter this code to confirm it's really you."
		subject = "Confirm linking your Sajni account"
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, map[string]any{
		"Greeting":         greeting,
		"Intro":            intro,
		"Code":             code,
		"ExpiresInMinutes": int(emailCodeTTL.Minutes()),
		"AppURL":           s.AppURL,
		"Name":             displayName,
	}); err != nil {
		return "", "", err
	}
	return subject, buf.String(), nil
}

// deliverCodeEmail renders the TOTP email and ships it. When
// RESEND_API_KEY is unset we log the code and no-op so local dev works.
func (s *Service) deliverCodeEmail(ctx context.Context, to, name, code, purpose string) error {
	subject, html, err := s.renderTOTPEmail(to, name, code, purpose)
	if err != nil {
		return err
	}
	if s.ResendAPIKey == "" {
		// Dev fallback: print the code to logs.
		fmt.Printf("[auth/email] (dev) RESEND_API_KEY unset — code for %s: %s\n", to, code)
		return nil
	}
	return s.SendEmail(ctx, to, subject, html)
}

// SendEmail ships an already-rendered HTML email through the Resend HTTP
// API. Generic sibling of deliverCodeEmail for non-auth senders (task
// reminders, etc). Dev fallback logs and no-ops when RESEND_API_KEY is
// unset so local dev never hard-fails on a missing key.
func (s *Service) SendEmail(ctx context.Context, to, subject, html string) error {
	if s.ResendAPIKey == "" {
		fmt.Printf("[email] (dev) RESEND_API_KEY unset — would send %q to %s\n", subject, to)
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"from":    s.EmailFrom,
		"to":      []string{to},
		"subject": subject,
		"html":    html,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.resend.com/emails", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.ResendAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("resend send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("resend send: %s — %s", resp.Status, string(b))
	}
	return nil
}

// SendTaskReminder renders + ships the task-reminder email. whenLabel is
// the event time already formatted in the user's tz by the caller (e.g.
// "today at 5:00 PM"); route is the in-app path the CTA opens (e.g.
// "/tasks"). Kept here so the AppURL + embedded template stay in one place.
func (s *Service) SendTaskReminder(ctx context.Context, to, name, taskTitle, whenLabel, route string) error {
	tpl, err := template.ParseFS(emailTemplatesFS, "email_templates/reminder.html")
	if err != nil {
		return err
	}
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		displayName = strings.SplitN(to, "@", 2)[0]
	}
	appURL := strings.TrimRight(s.AppURL, "/")
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, map[string]any{
		"Name":      displayName,
		"TaskTitle": taskTitle,
		"WhenLabel": whenLabel,
		"AppURL":    appURL,
		"CTAURL":    appURL + route,
	}); err != nil {
		return err
	}
	subject := "Reminder: " + taskTitle
	return s.SendEmail(ctx, to, subject, buf.String())
}

// consumeEmailCode verifies a 6-digit code for the given email + purpose.
// On success returns the matched row's metadata so the handler can act
// on link payloads. Increments attempts on mismatch; locks at 5.
type consumedCode struct {
	ID           string
	Purpose      string
	LinkUserID   string
	LinkProvider string
	LinkSubject  string
	LinkName     string
}

func (s *Service) consumeEmailCode(ctx context.Context, email, code string) (*consumedCode, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT id, code_hash, purpose,
		       COALESCE(link_user_id::text,''),
		       COALESCE(link_provider,''),
		       COALESCE(link_subject,''),
		       COALESCE(link_name,''),
		       attempts, expires_at
		  FROM email_codes
		 WHERE email=$1 AND consumed_at IS NULL
		 ORDER BY created_at DESC LIMIT 1`, email)
	var (
		id, purpose, luid, lprov, lsub, lname string
		stored                                []byte
		attempts                              int
		expires                               time.Time
	)
	if err := row.Scan(&id, &stored, &purpose, &luid, &lprov, &lsub, &lname, &attempts, &expires); err != nil {
		return nil, errors.New("no active code")
	}
	if time.Now().After(expires) {
		return nil, errors.New("code expired")
	}
	if attempts >= emailCodeAttempts {
		return nil, errors.New("too many attempts")
	}
	if !bytesEqual(stored, hashCode(code)) {
		s.DB.ExecContext(ctx, `UPDATE email_codes SET attempts = attempts + 1 WHERE id=$1`, id)
		return nil, errors.New("wrong code")
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE email_codes SET consumed_at=NOW() WHERE id=$1`, id); err != nil {
		return nil, err
	}
	return &consumedCode{
		ID:           id,
		Purpose:      purpose,
		LinkUserID:   luid,
		LinkProvider: lprov,
		LinkSubject:  lsub,
		LinkName:     lname,
	}, nil
}

// bytesEqual is a constant-time-ish compare; not strictly needed for
// SHA-256 digests but cheap insurance against timing oracles.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
