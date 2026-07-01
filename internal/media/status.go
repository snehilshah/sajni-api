package media

import "strings"

// Status is the canonical media-library status stored in Postgres and
// returned to clients. Labels such as "Complete" are frontend concerns.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusWaiting    Status = "waiting"
	StatusComplete   Status = "complete"
	StatusUpcoming   Status = "upcoming"
	StatusDropped    Status = "dropped"
	StatusScratched  Status = "scratched"
	StatusArchived   Status = "archived"
)

// NormalizeStatus accepts canonical values plus old AI/client aliases.
// It deliberately maps task-style "done" to media-style "complete".
func NormalizeStatus(raw string) (Status, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return StatusPending, true
	case string(StatusPending), "planned", "plan", "queue", "queued", "want_to_watch", "want-to-watch":
		return StatusPending, true
	case string(StatusInProgress), "in progress", "in-progress", "watching", "reading", "started":
		return StatusInProgress, true
	case string(StatusWaiting):
		return StatusWaiting, true
	case string(StatusComplete), "completed", "done", "finished", "watched", "read":
		return StatusComplete, true
	case string(StatusUpcoming):
		return StatusUpcoming, true
	case string(StatusDropped), "drop":
		return StatusDropped, true
	case string(StatusScratched), "scratch":
		return StatusScratched, true
	case string(StatusArchived), "archive":
		return StatusArchived, true
	default:
		return "", false
	}
}
