package reminderqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	KindTask  = "task"
	KindMulti = "multi"
)

type fireBody struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

type config struct {
	project string
	region  string
	queue   string
	baseURL string
	secret  string
}

func loadConfig() config {
	project := firstEnv("CLOUD_TASKS_PROJECT", "GOOGLE_CLOUD_PROJECT", "GCP_PROJECT_ID")
	return config{
		project: project,
		region:  defaultEnv("CLOUD_TASKS_LOCATION", "asia-south1"),
		queue:   os.Getenv("CLOUD_TASKS_QUEUE"),
		baseURL: strings.TrimRight(os.Getenv("API_BASE_URL"), "/"),
		secret:  os.Getenv("REMINDER_CRON_SECRET"),
	}
}

func (c config) enabled() bool {
	return c.project != "" && c.region != "" && c.queue != "" && c.baseURL != "" && c.secret != ""
}

func (c config) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s/queues/%s", c.project, c.region, c.queue)
}

func (c config) fireURL() string {
	u, err := url.JoinPath(c.baseURL, "/internal/reminders/fire")
	if err != nil {
		return c.baseURL + "/internal/reminders/fire"
	}
	return u
}

// Enqueue schedules one reminder fire. It is intentionally append-only:
// edits enqueue a new Cloud Task, and stale older tasks no-op after the fire
// endpoint re-checks Postgres.
func Enqueue(ctx context.Context, kind string, id int64, at time.Time) error {
	if kind != KindTask && kind != KindMulti {
		return fmt.Errorf("unknown reminder kind %q", kind)
	}
	if id <= 0 || at.IsZero() {
		return nil
	}
	c := loadConfig()
	if !c.enabled() {
		log.Debug().Str("kind", kind).Int64("id", id).Msg("reminder cloud task enqueue skipped; config incomplete")
		return nil
	}
	body, err := json.Marshal(fireBody{Kind: kind, ID: id})
	if err != nil {
		return err
	}
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		return err
	}
	defer client.Close()

	_, err = client.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: c.parent(),
		Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{
				HttpRequest: &cloudtaskspb.HttpRequest{
					Url:        c.fireURL(),
					HttpMethod: cloudtaskspb.HttpMethod_POST,
					Headers: map[string]string{
						"Content-Type":    "application/json",
						"X-Reminder-Cron": c.secret,
					},
					Body: body,
				},
			},
			ScheduleTime:     timestamppb.New(at),
			DispatchDeadline: durationpb.New(30 * time.Second),
		},
	})
	return err
}

func EnqueueTask(ctx context.Context, id int64, scheduledAt time.Time) error {
	return Enqueue(ctx, KindTask, id, scheduledAt)
}

func EnqueueMulti(ctx context.Context, id int64, remindAt time.Time) error {
	return Enqueue(ctx, KindMulti, id, remindAt)
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func defaultEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
