package reminderqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"sajni/internal/config"
)

const (
	KindTask       = "task"
	KindMulti      = "multi"
	KindStandalone = "standalone"
)

type fireBody struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

type Queue struct {
	project string
	region  string
	queue   string
	baseURL string
	secret  string
}

func New(cfg config.Reminders, apiBaseURL string) Queue {
	queue := Queue{
		project: cfg.CloudTasksProject,
		region:  cfg.CloudTasksLocation,
		queue:   cfg.CloudTasksQueue,
		baseURL: strings.TrimRight(apiBaseURL, "/"),
		secret:  cfg.CronSecret,
	}
	return queue
}

func (c Queue) enabled() bool {
	return c.project != "" && c.region != "" && c.queue != "" && c.baseURL != "" && c.secret != ""
}

func (c Queue) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s/queues/%s", c.project, c.region, c.queue)
}

func (c Queue) fireURL() string {
	u, err := url.JoinPath(c.baseURL, "/internal/reminders/fire")
	if err != nil {
		return c.baseURL + "/internal/reminders/fire"
	}
	return u
}

// Enqueue schedules one reminder fire. It is intentionally append-only:
// edits enqueue a new Cloud Task, and stale older tasks no-op after the fire
// endpoint re-checks Postgres.
func (c Queue) Enqueue(ctx context.Context, kind string, id int64, at time.Time) error {
	if kind != KindTask && kind != KindMulti && kind != KindStandalone {
		return fmt.Errorf("unknown reminder kind %q", kind)
	}
	if id <= 0 || at.IsZero() {
		return nil
	}
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

func (c Queue) EnqueueTask(ctx context.Context, id int64, scheduledAt time.Time) error {
	return c.Enqueue(ctx, KindTask, id, scheduledAt)
}

func (c Queue) EnqueueMulti(ctx context.Context, id int64, remindAt time.Time) error {
	return c.Enqueue(ctx, KindMulti, id, remindAt)
}

func (c Queue) EnqueueStandalone(ctx context.Context, id int64, fireAt time.Time) error {
	return c.Enqueue(ctx, KindStandalone, id, fireAt)
}
