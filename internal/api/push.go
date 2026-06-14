package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"

	"sajni/internal/push"
)

// Push device registration. The android app POSTs its FCM token after
// sign-in and on token rotation; sign-out unregisters it. Delivery policy
// lives in the per-kind senders: notifications go over BOTH channels — push
// to every live device, plus the email where one exists for that kind.

func registerPushRoutes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /api/push/register", registerPushDevice(deps))
	mux.HandleFunc("POST /api/push/unregister", unregisterPushDevice(deps))
}

func registerPushDevice(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Token    string `json:"token"`
			Platform string `json:"platform"`
		}
		if err := readJSON(r, &body); err != nil || strings.TrimSpace(body.Token) == "" {
			errJSON(w, 400, "token required")
			return
		}
		if body.Platform == "" {
			body.Platform = "android"
		}
		// Token is the PK: a token that re-registers (same device, new
		// sign-in) simply moves to the current user.
		_, err := d.ExecContext(r.Context(), `
			INSERT INTO push_devices (token, user_id, platform)
			VALUES ($1, $2, $3)
			ON CONFLICT (token)
			DO UPDATE SET user_id = EXCLUDED.user_id, platform = EXCLUDED.platform, last_seen_at = NOW()`,
			body.Token, uid, body.Platform)
		if err != nil {
			errJSON(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

func unregisterPushDevice(deps Deps) http.HandlerFunc {
	d := deps.DB
	return func(w http.ResponseWriter, r *http.Request) {
		uid := userID(r.Context())
		var body struct {
			Token string `json:"token"`
		}
		if err := readJSON(r, &body); err != nil || body.Token == "" {
			errJSON(w, 400, "token required")
			return
		}
		d.ExecContext(r.Context(), `DELETE FROM push_devices WHERE token = $1 AND user_id = $2`, body.Token, uid)
		writeJSON(w, 200, map[string]string{"status": "ok"})
	}
}

// notifyPush pushes to every live device of uid and reports whether at least
// one delivery landed. Callers send email regardless; the bool only matters
// for "did ANY channel deliver" success accounting.
func notifyPush(ctx context.Context, deps Deps, uid string, n push.Notification) bool {
	if deps.Push == nil {
		return false
	}
	sent, err := deps.Push.SendToUser(ctx, deps.DB, uid, n)
	if err != nil {
		log.Warn().Err(err).Str("user", uid).Msg("push lookup failed")
		return false
	}
	return sent > 0
}
