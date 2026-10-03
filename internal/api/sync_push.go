package api

import (
	"context"
	"net/http"
	"time"

	"sajni/internal/push"
)

// NotifySync tells the user's native devices that data in scope changed
// ("habits"), so surfaces that cache it — home-screen widgets — refetch
// instead of polling. Silent, normal priority and collapsible (see
// push.TypeSync); fire-and-forget so the request never waits on FCM.
func NotifySync(deps Deps, uid, scope string) {
	if deps.Push == nil || uid == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = deps.Push.SendToUser(ctx, deps.DB, uid, push.Notification{
			Type: push.TypeSync,
			Data: map[string]string{"scope": scope},
		})
	}()
}

// withSync wraps a mutating handler: a 2xx response pings the user's
// devices for scope.
func withSync(deps Deps, scope string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h(sw, r)
		if sw.status < 300 {
			NotifySync(deps, userID(r.Context()), scope)
		}
	}
}
