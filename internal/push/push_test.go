package push

import "testing"

func TestBuildMessageNotificationBlocks(t *testing.T) {
	cases := []struct {
		name             string
		n                Notification
		wantNotification bool
	}{
		{"task reminder", Notification{Type: TypeTaskReminder, Title: "Reminder", Body: "Pay rent"}, true},
		{"digest", Notification{Type: TypeTaskDigest, Title: "Week", Body: "3 pending"}, true},
		{"standalone reminder", Notification{Type: TypeReminder, Title: "Reminder", Body: "Buy pass", DataOnly: true}, false},
		{"sync", Notification{Type: TypeSync, Data: map[string]string{"scope": "habits"}, DataOnly: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := buildMessage("tok", tc.n)
			_, top := msg["notification"]
			_, android := msg["android"].(map[string]any)["notification"]
			if top != tc.wantNotification || android != tc.wantNotification {
				t.Fatalf("notification=%v android.notification=%v, want both %v", top, android, tc.wantNotification)
			}
			data := msg["data"].(map[string]string)
			if data["title"] != tc.n.Title || data["body"] != tc.n.Body {
				t.Fatalf("data title/body = %q/%q", data["title"], data["body"])
			}
		})
	}
	if ch := buildMessage("tok", Notification{Type: TypeBillDue, Title: "t", Body: "b"})["android"].(map[string]any)["notification"].(map[string]string)["channel_id"]; ch != "bills" {
		t.Fatalf("bill channel = %q", ch)
	}
}
