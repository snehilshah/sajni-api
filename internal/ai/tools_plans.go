package ai

import (
	"context"
	"fmt"

	"google.golang.org/genai"

	"sajni/internal/db"
	"sajni/internal/plan"
	"sajni/internal/reminderqueue"
	"sajni/internal/taskrepeat"
)

// Plan tools mirror internal/api/plans.go through internal/plan. A plan's
// tasks are tasks: create_task with plan_id adds a new one, add_task_to_plan
// pulls in an existing one.
func planTools(d *db.DB, queue reminderqueue.Queue) []Tool {
	return []Tool{
		{
			Name:        "list_plans",
			Description: "List the user's Journal plans: repeating moments like 'Office start' on workdays at 09:00, where the user plans what that day is about. Pass date to get only the plans that run that day, each with its planned tasks and left-over (unfinished) tasks from earlier days.",
			Schema: obj(map[string]*genai.Schema{
				"date": str("Optional local date YYYY-MM-DD for that day's plans."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				if date := argStr(args, "date"); date != "" {
					items, err := plan.Day(ctx, d, uid, date)
					return map[string]any{"items": items, "count": len(items)}, nil, err
				}
				items, err := plan.List(ctx, d, uid)
				return map[string]any{"items": items, "count": len(items)}, nil, err
			},
		},
		{
			Name:        "save_plan",
			Description: "Create a Journal plan, or edit one when id is given (only the fields you pass change). A plan is a named moment that repeats on weekdays at a start time; it alerts once at the start time unless notify is false. To put tasks in a plan use create_task with plan_id or add_task_to_plan.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"id":         intg("Optional plan id to edit, from list_plans."),
				"name":       str("Name, e.g. 'Office start'. Required when creating."),
				"weekdays":   arrayOf(intg("0=Sunday through 6=Saturday."), "Days it runs. Default Monday to Friday."),
				"start_time": str("Local start time HH:MM, default 09:00."),
				"notify":     boolean("Alert at the start time. Default true."),
				"color":      str("Optional hex accent like #2D5A4F."),
			}),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				in := plan.SaveInput{}
				if id > 0 {
					cur, err := plan.Get(ctx, d, uid, id)
					if err != nil {
						return nil, nil, err
					}
					notify := cur.Notify
					in = plan.SaveInput{Name: cur.Name, Color: cur.Color, Weekdays: cur.Weekdays, StartTime: cur.StartTime, Notify: &notify}
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
				p, err := plan.Save(ctx, d, queue, uid, id, in)
				if err != nil {
					return nil, nil, err
				}
				kind := "plan_updated"
				if id == 0 {
					kind = "plan_created"
				}
				return p, planMeta(kind, p.ID, p.Name, userTZNow(ctx, d, uid).Format("2006-01-02")), nil
			},
		},
		{
			Name:        "delete_plan",
			Description: "Delete a Journal plan. Its tasks stay as plain tasks.",
			Mutating:    true,
			Schema:      obj(map[string]*genai.Schema{"id": intg("Required plan id.")}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				if err := plan.Delete(ctx, d, uid, id); err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "deleted": true},
					planMeta("plan_deleted", id, "", userTZNow(ctx, d, uid).Format("2006-01-02")), nil
			},
		},
		{
			Name:        "add_task_to_plan",
			Description: "Put an existing task into a plan's occurrence: it moves to that date (default the plan's next occurrence), keeping any clock time. Use list_tasks to find the task and list_plans for the plan.",
			Mutating:    true,
			Schema: obj(map[string]*genai.Schema{
				"plan_id": intg("Required plan id."),
				"task_id": intg("Required task id."),
				"date":    str("Optional local date YYYY-MM-DD; must be a day the plan runs."),
			}, "plan_id", "task_id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				pid, tid := argInt(args, "plan_id", 0), argInt(args, "task_id", 0)
				date, err := plan.AddTask(ctx, d, queue, uid, pid, tid, argStr(args, "date"))
				if err != nil {
					return nil, nil, err
				}
				return map[string]any{"plan_id": pid, "task_id": tid, "date": date},
					planMeta("task_updated", tid, "", date), nil
			},
		},
		{
			Name:        "skip_task_occurrence",
			Description: "Skip this time for a repeating task: it moves to its next date without counting as missed, with its steps unticked.",
			Mutating:    true,
			Schema:      obj(map[string]*genai.Schema{"id": intg("Required id of a repeating task.")}, "id"),
			Handler: func(ctx context.Context, uid string, args map[string]any) (any, map[string]any, error) {
				id := argInt(args, "id", 0)
				m, err := taskrepeat.Skip(ctx, d, queue, uid, id)
				if err != nil {
					return nil, nil, err
				}
				return map[string]any{"id": id, "skipped": m.From, "next": m.To},
					map[string]any{"kind": "task_updated", "id": id, "title": m.Title, "route": fmt.Sprintf("/tasks?focus=%d", id)}, nil
			},
		},
	}
}

func planMeta(kind string, id int64, title, date string) map[string]any {
	return map[string]any{"kind": kind, "id": id, "title": title, "route": fmt.Sprintf("/journal?date=%s", date)}
}

// taskRepeatSchema is the repeat rule a task takes; week tasks repeat
// weekly and month goals monthly, every `interval`.
func taskRepeatSchema() *genai.Schema {
	return obj(map[string]*genai.Schema{
		"frequency":    str("'daily' | 'weekly' | 'monthly' | 'yearly'. Week tasks take 'weekly', month goals 'monthly'."),
		"interval":     intg("Every N frequency units; default 1."),
		"weekdays":     arrayOf(intg("0=Sunday through 6=Saturday."), "For weekly day tasks: the days, e.g. [1,2,3,4,5] for workdays."),
		"monthly_mode": str("For monthly day tasks: 'date' or 'weekday'."),
		"month_day":    intg("For monthly date rules: 1 through 31."),
		"weekday":      intg("For monthly weekday rules: 0=Sunday through 6=Saturday."),
		"ordinal":      intg("For monthly weekday rules: 1 through 5, or -1 for last."),
	})
}
