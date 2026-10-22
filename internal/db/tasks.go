package db

import (
	"context"
	"database/sql"
	"fmt"

	"sajni/internal/storage"
)

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// OpenSubtaskCount counts every still-open descendant (any depth) of a task.
// A task can't be marked done while this is non-zero; scratched children
// count as closed.
func OpenSubtaskCount(ctx context.Context, q rowQueryer, userID string, taskID int64) int {
	var n int
	q.QueryRowContext(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id, status FROM tasks WHERE parent_task_id = $1 AND user_id = $2
			UNION ALL
			SELECT t.id, t.status FROM tasks t JOIN tree ON t.parent_task_id = tree.id
			 WHERE t.user_id = $2
		)
		SELECT COUNT(*) FROM tree WHERE status NOT IN ('done','scratched')`, taskID, userID).Scan(&n)
	return n
}

// OpenSubtasksMessage is the user-facing refusal shared by the API and AI.
func OpenSubtasksMessage(n int) string {
	if n == 1 {
		return "Finish its 1 open subtask first"
	}
	return fmt.Sprintf("Finish its %d open subtasks first", n)
}

// DeleteTaskFiles removes the stored images attached to a task and its
// subtasks. Call it before deleting the task; the rows cascade with it.
func (d *DB) DeleteTaskFiles(ctx context.Context, store storage.Storage, userID string, taskID int64) {
	rows, err := d.QueryContext(ctx, `
		WITH RECURSIVE tree AS (
			SELECT id FROM tasks WHERE id = $1 AND user_id = $2
			UNION ALL
			SELECT t.id FROM tasks t JOIN tree ON t.parent_task_id = tree.id WHERE t.user_id = $2
		)
		SELECT a.filename FROM task_attachments a JOIN tree ON a.task_id = tree.id`, taskID, userID)
	if err != nil {
		return
	}
	var files []string
	for rows.Next() {
		var f string
		if rows.Scan(&f) == nil {
			files = append(files, f)
		}
	}
	rows.Close()
	for _, f := range files {
		store.Delete(ctx, storage.UserKey(userID, "uploads", f))
	}
}
