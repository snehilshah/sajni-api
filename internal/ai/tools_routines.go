package ai

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"

	"sajni/internal/db"
	"sajni/internal/reminderqueue"
	"sajni/internal/routine"
)

// Routine tools mirror internal/api/routines.go through internal/routine.
// Agenda items are tasks: create_task with routine_id adds one.
func routineTools(d *db.DB, queue reminderqueue.Queue) []Tool {
	return []Tool{
		{
			Name:        "list_routines",
			Description: "List the user's Journal routines (recurring time slots like 'Office start' on workdays at 09:00) with their standing checklist. Pass date to also get that day's ticked items and agenda tasks; only routines that run that day are returned then.",
			Schema: obj(map[string]*genai.Schema{
				"date": str("Optional local date YYYY-MM-DD for that day's state."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				if date := argStr(args, "date"); date != "" {
					items, err := routine.Day(ctx, d, uid, date)
					return map[string]any{"items": items, "count": len(items)}, nil, err
				}
				items, err := routine.List(ctx, d, uid)
				return map[string]any{"items": items, "count": len(items)}, nil, err
			},
		},
		{
			Name:        "save_routine",
			Description: "Create a Journal routine, or edit one when id is given (only the fields you pass change). A routine is a named slot that repeats on weekdays at a start time, with a standing checklist that starts fresh each day. It alerts once at the start time unless notify is false. For one-off things to do in a routine, use create_task with routine_id instead.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":         intg("Optional routine id to edit, from list_routines."),
				"name":       str("Name, e.g. 'Office start'. Required when creating."),
				"weekdays":   arrayOf(intg("0=Sunday through 6=Saturday."), "Days it runs. Default Monday to Friday."),
				"start_time": str("Local start time HH:MM, default 09:00."),
				"notify":     boolean("Alert at the start time. Default true."),
				"items":      arrayOf(str(""), "The standing checklist; replaces the current one when given."),
				"color":      str("Optional hex accent like #2D5A4F."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				in := routine.SaveInput{}
				if id > 0 {
					cur, err := routine.Get(ctx, d, uid, id)
					if err != nil {
						return nil, nil, err
					}
					notify := cur.Notify
					in = routine.SaveInput{Name: cur.Name, Color: cur.Color, Weekdays: cur.Weekdays,
						StartTime: cur.StartTime, Notify: &notify, Items: cur.Items}
				}
				if v := argStr(args, "name"); v != "" {
					in.Name = v
				}
				if v, ok := args["color"].(string); ok {
					in.Color = v
				}
				if v := argStr(args, "start_time"); v != "" {
					in.StartTime = v
				}
				if v, ok := args["notify"].(bool); ok {
					in.Notify = &v
				}
				if raw, ok := args["weekdays"].([]any); ok {
					in.Weekdays = []int{}
					for _, day := range raw {
						in.Weekdays = append(in.Weekdays, int(argInt(map[string]any{"d": day}, "d", -1)))
					}
				}
				if _, ok := args["items"]; ok {
					// Keep the ids of lines that survive, so today's ticks stay.
					byText := map[string]string{}
					for _, it := range in.Items {
						byText[strings.ToLower(it.Text)] = it.ID
					}
					in.Items = []routine.Item{}
					for _, text := range argStrSlice(args, "items") {
						in.Items = append(in.Items, routine.Item{ID: byText[strings.ToLower(strings.TrimSpace(text))], Text: text})
					}
				}
				r, err := routine.Save(ctx, d, queue, uid, id, in)
				if err != nil {
					return nil, nil, err
				}
				kind := "routine_updated"
				if id == 0 {
					kind = "routine_created"
				}
				return r, routineMeta(kind, r.ID, r.Name, userTZNow(ctx, d, uid).Format("2006-01-02")), nil
			},
		},
		{
			Name:        "delete_routine",
			Description: "Delete a Journal routine. Its agenda tasks stay as plain tasks.",
			Mutating:    true,
			Schema:      obj(map[string]*genai.Schema{"id": intg("Required routine id.")}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				if err := routine.Delete(ctx, d, uid, id); err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "deleted": true},
					routineMeta("routine_deleted", id, "", userTZNow(ctx, d, uid).Format("2006-01-02")), nil
			},
		},
		{
			Name:        "check_routine_item",
			Description: "Tick (or untick) one standing checklist item of a routine for one day. Get item ids from list_routines.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":      intg("Required routine id."),
				"item_id": str("Required checklist item id."),
				"date":    str("Local date YYYY-MM-DD; default today."),
				"done":    boolean("false to untick. Default true."),
			}, "id", "item_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id, item := argInt(args, "id", 0), argStr(args, "item_id")
				date := argStr(args, "date")
				if date == "" {
					date = userTZNow(ctx, d, uid).Format("2006-01-02")
				}
				done := argBool(args, "done", true)
				if err := routine.Check(ctx, d, uid, id, date, item, done); err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "item_id": item, "date": date, "done": done},
					routineMeta("routine_checked", id, "", date), nil
			},
		},
	}
}

func routineMeta(kind string, id int64, title, date string) map[string]any {
	return map[string]any{"kind": kind, "id": id, "title": title, "route": fmt.Sprintf("/journal?date=%s", date)}
}
