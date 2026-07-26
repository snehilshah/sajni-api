package api

import (
	"context"
	"errors"
	"net/http"
	"os"
)

// RegisterScheduledNotificationHandler mounts the one Cloud Scheduler target
// for date-driven user notifications. Exact task reminders still use Cloud
// Tasks; this sweep covers local-calendar work and is safe to retry.
func RegisterScheduledNotificationHandler(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("POST /internal/notifications/run", scheduledNotificationHandler(deps))
}

func scheduledNotificationHandler(deps Deps) http.HandlerFunc {
	expected := os.Getenv("REMINDER_CRON_SECRET")
	return func(w http.ResponseWriter, r *http.Request) {
		if expected == "" || r.Header.Get("X-Reminder-Cron") != expected {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		result, err := ProcessScheduledNotifications(r.Context(), deps)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// ProcessScheduledNotifications intentionally runs every processor even when
// one fails. Their database stamps/unique keys are independent, so a biller
// error must not prevent a movie email or a task digest for another user.
func ProcessScheduledNotifications(ctx context.Context, deps Deps) (map[string]int, error) {
	result := map[string]int{}
	var jobErrors []error

	reminded, graduated, err := ProcessMediaReleaseCron(ctx, deps)
	result["movie_reminders"] = reminded
	result["movies_graduated"] = graduated
	if err != nil {
		jobErrors = append(jobErrors, err)
	}

	weekly, monthly, err := ProcessDigestCron(ctx, deps)
	result["weekly_digests"] = weekly
	result["monthly_digests"] = monthly
	if err != nil {
		jobErrors = append(jobErrors, err)
	}

	autoPaid, billerAlerts, err := ProcessBillerCron(ctx, deps)
	result["billers_auto_paid"] = autoPaid
	result["biller_alerts"] = billerAlerts
	if err != nil {
		jobErrors = append(jobErrors, err)
	}

	investments, err := ProcessInvestmentDebits(ctx, deps)
	result["investment_debits"] = investments
	if err != nil {
		jobErrors = append(jobErrors, err)
	}

	return result, errors.Join(jobErrors...)
}
