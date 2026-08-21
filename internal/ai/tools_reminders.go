package ai

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"

	"sajni/internal/db"
	standalonereminder "sajni/internal/reminder"
	"sajni/internal/reminderqueue"
)

func standaloneReminderTools(d *db.DB, queue reminderqueue.Queue) []Tool {
	return []Tool{
		{
			Name:        "list_reminders",
			Description: "List standalone personal reminders. These are lightweight notifications, not tasks. Use before editing, snoozing, skipping, or deleting a reminder. Upcoming items include their next occurrence; include_recent adds delivery/skip history.",
			Schema: obj(map[string]*genai.Schema{
				"search":         str("Optional text search across reminder message and notes."),
				"include_recent": boolean("Include recent delivered/skipped occurrences."),
				"limit":          intg("Maximum active reminders; default 100."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				items, err := standalonereminder.List(ctx, d, uid, argStr(args, "search"), true, int(argInt(args, "limit", 100)))
				if err != nil {
					return nil, nil, err
				}
				out := map[string]any{"upcoming": items}
				if argBool(args, "include_recent", false) {
					history, err := standalonereminder.History(ctx, d, uid, 30, 0)
					if err != nil {
						return nil, nil, err
					}
					out["recent"] = history
				}
				return out, nil, nil
			},
		},
		{
			Name:        "create_reminder",
			Description: "Create a standalone personal reminder. Use for explicit wording such as 'remind me to call Mom tomorrow at 4'. Do not use create_task for these. starts_at is required: if the user gave a date but no clock time, ask one short follow-up before calling. Resolve relative dates against get_current_context. Supports practical recurrence via recurrence; omit it for one-time reminders.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"message":    str("Required reminder message."),
				"notes":      str("Optional supporting context."),
				"starts_at":  str("Required ISO timestamp with offset, e.g. 2026-08-22T16:00:00+05:30."),
				"timezone":   str("Optional IANA timezone. Defaults to the user's timezone."),
				"recurrence": reminderRecurrenceSchema(),
			}, "message", "starts_at"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				item, err := standalonereminder.Create(ctx, d, queue, uid, reminderInputFromArgs(args))
				if err != nil {
					return nil, nil, err
				}
				return item, reminderMeta("reminder_created", item), nil
			},
		},
		{
			Name:        "update_reminder",
			Description: "Edit an entire standalone reminder series. Only passed fields change. Changing starts_at or recurrence replaces the pending occurrence but keeps delivered history. Use this for deliberate series changes; use snooze_reminder for a temporary occurrence-only delay.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":               intg("Required reminder id from list_reminders."),
				"message":          str("Optional new reminder message."),
				"notes":            str("Optional new notes."),
				"starts_at":        str("Optional new ISO timestamp with offset."),
				"timezone":         str("Optional IANA timezone."),
				"recurrence":       reminderRecurrenceSchema(),
				"clear_recurrence": boolean("Set true to make the reminder one-time."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				current, err := standalonereminder.Get(ctx, d, uid, id)
				if err != nil {
					return nil, nil, err
				}
				anchor := current.StartsAt
				if current.NextOccurrence != nil {
					anchor = current.NextOccurrence.ScheduledAt
				}
				input := standalonereminder.SaveInput{
					Message: current.Message, Notes: current.Notes, Timezone: current.Timezone,
					StartsAt: anchor.Format(time.RFC3339), Recurrence: current.Recurrence,
				}
				if _, ok := args["message"]; ok {
					input.Message = argStr(args, "message")
				}
				if _, ok := args["notes"]; ok {
					input.Notes = argStr(args, "notes")
				}
				if _, ok := args["starts_at"]; ok {
					input.StartsAt = argStr(args, "starts_at")
				}
				if _, ok := args["timezone"]; ok {
					input.Timezone = argStr(args, "timezone")
				}
				if raw, ok := args["recurrence"]; ok && raw != nil {
					input.Recurrence = reminderRuleFromAny(raw)
				}
				if argBool(args, "clear_recurrence", false) {
					input.Recurrence = standalonereminder.Rule{}
				}
				item, err := standalonereminder.Replace(ctx, d, queue, uid, id, input)
				if err != nil {
					return nil, nil, err
				}
				return item, reminderMeta("reminder_updated", item), nil
			},
		},
		{
			Name:        "snooze_reminder",
			Description: "Delay only the current occurrence of a standalone reminder. For a recurring reminder the future cadence stays unchanged. Pass either minutes or fire_at.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":            intg("Required reminder id."),
				"occurrence_id": intg("Optional current occurrence id from list_reminders."),
				"minutes":       intg("Delay from now in minutes."),
				"fire_at":       str("Specific new ISO timestamp with offset."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				var fireAt time.Time
				var err error
				if value := strings.TrimSpace(argStr(args, "fire_at")); value != "" {
					fireAt, err = time.Parse(time.RFC3339, value)
				} else if minutes := argInt(args, "minutes", 0); minutes > 0 {
					fireAt = time.Now().Add(time.Duration(minutes) * time.Minute)
				} else {
					err = fmt.Errorf("minutes or fire_at required")
				}
				if err != nil {
					return nil, nil, err
				}
				item, err := standalonereminder.Snooze(ctx, d, queue, uid, id, argInt(args, "occurrence_id", 0), fireAt)
				if err != nil {
					return nil, nil, err
				}
				return item, reminderMeta("reminder_snoozed", item), nil
			},
		},
		{
			Name:        "skip_reminder_occurrence",
			Description: "Skip the current occurrence of a recurring standalone reminder without changing its future cadence. A one-time reminder becomes inactive.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":            intg("Required reminder id."),
				"occurrence_id": intg("Optional current occurrence id."),
			}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				item, err := standalonereminder.Skip(ctx, d, queue, uid, argInt(args, "id", 0), argInt(args, "occurrence_id", 0))
				if err != nil {
					return nil, nil, err
				}
				return item, reminderMeta("reminder_skipped", item), nil
			},
		},
		{
			Name:        "delete_reminder",
			Description: "Permanently delete an entire standalone reminder series and its occurrence history.",
			Mutating:    true,
			Schema:      obj(map[string]*genai.Schema{"id": intg("Required reminder id.")}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				if err := standalonereminder.Delete(ctx, d, uid, id); err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "deleted": true}, map[string]any{
					"kind": "reminder_deleted", "id": id, "route": "/tasks?tab=reminders",
				}, nil
			},
		},
	}
}

func reminderRecurrenceSchema() *genai.Schema {
	return obj(map[string]*genai.Schema{
		"frequency":    str("'daily' | 'weekly' | 'monthly' | 'yearly'. Omit the recurrence object for one-time reminders."),
		"interval":     intg("Every N frequency units; default 1."),
		"weekdays":     arrayOf(intg("0=Sunday through 6=Saturday."), "For weekly rules, selected weekdays."),
		"monthly_mode": str("For monthly rules: 'date' or 'weekday'."),
		"month_day":    intg("For monthly date rules: 1 through 31."),
		"weekday":      intg("For monthly weekday rules: 0=Sunday through 6=Saturday."),
		"ordinal":      intg("For monthly weekday rules: 1 through 5, or -1 for last."),
		"until":        str("Optional inclusive local end date YYYY-MM-DD."),
		"count":        intg("Optional maximum number of occurrences."),
	})
}

func reminderInputFromArgs(args map[string]any) standalonereminder.SaveInput {
	input := standalonereminder.SaveInput{
		Message: argStr(args, "message"), Notes: argStr(args, "notes"),
		StartsAt: argStr(args, "starts_at"), Timezone: argStr(args, "timezone"),
	}
	if raw, ok := args["recurrence"]; ok && raw != nil {
		input.Recurrence = reminderRuleFromAny(raw)
	}
	return input
}

func reminderRuleFromAny(raw any) standalonereminder.Rule {
	values, _ := raw.(map[string]any)
	rule := standalonereminder.Rule{
		Frequency: argStr(values, "frequency"), Interval: int(argInt(values, "interval", 0)),
		MonthlyMode: argStr(values, "monthly_mode"), MonthDay: int(argInt(values, "month_day", 0)),
		Weekday: int(argInt(values, "weekday", 0)), Ordinal: int(argInt(values, "ordinal", 0)),
		Until: argStr(values, "until"), Count: int(argInt(values, "count", 0)),
	}
	if weekdays, ok := values["weekdays"].([]any); ok {
		for _, rawDay := range weekdays {
			rule.Weekdays = append(rule.Weekdays, int(argInt(map[string]any{"day": rawDay}, "day", -1)))
		}
	}
	return rule
}

func reminderMeta(kind string, item standalonereminder.Reminder) map[string]any {
	return map[string]any{
		"kind": kind, "id": item.ID, "title": item.Message,
		"route": fmt.Sprintf("/tasks?tab=reminders&focus=%d", item.ID),
	}
}
