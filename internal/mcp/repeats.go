package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// repeatWindow is how long a change counts as "just made": a resend inside
// it (an app retrying a slow reply) returns the first answer instead of
// making the change twice.
const repeatWindow = 30 * time.Second

// repeatNote rides on a dropped resend so the assistant knows why.
const repeatNote = "This exact change was made moments ago, so it wasn't repeated. To make it twice on purpose, change something (a note or the time)."

type repeats struct {
	mu   sync.Mutex
	seen map[string]*repeat
}

type repeat struct {
	at   time.Time
	done chan struct{}
	res  map[string]any
}

// dedupeKey identifies one change: user, tool and arguments (JSON objects
// encode with sorted keys, so argument order doesn't matter).
func dedupeKey(uid, tool string, args map[string]any) string {
	b, _ := json.Marshal(args)
	sum := sha256.Sum256([]byte(uid + "\x00" + tool + "\x00" + string(b)))
	return hex.EncodeToString(sum[:])
}

// claim reports whether key was claimed within the window. If so, it waits
// for that call to finish and returns its answer with a note; otherwise the
// caller runs the change and must call finish.
func (r *repeats) claim(key string) (map[string]any, bool) {
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[string]*repeat{}
	}
	now := time.Now()
	for k, e := range r.seen {
		if now.Sub(e.at) > repeatWindow {
			select {
			case <-e.done:
				delete(r.seen, k)
			default: // still running; keep it
			}
		}
	}
	if e, ok := r.seen[key]; ok {
		r.mu.Unlock()
		<-e.done
		if e.res == nil || e.res["isError"] == true {
			// The first try failed: let this one run.
			return nil, false
		}
		return withNote(e.res), true
	}
	r.seen[key] = &repeat{at: now, done: make(chan struct{})}
	r.mu.Unlock()
	return nil, false
}

func (r *repeats) finish(key string, res map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.seen[key]; ok {
		select {
		case <-e.done:
		default:
			e.res = res
			close(e.done)
		}
	}
	if res == nil || res["isError"] == true {
		delete(r.seen, key)
	}
}

func withNote(res map[string]any) map[string]any {
	out := map[string]any{"isError": false}
	var content []map[string]any
	if c, ok := res["content"].([]map[string]any); ok {
		content = append(content, c...)
	}
	content = append(content, map[string]any{"type": "text", "text": repeatNote})
	out["content"] = content
	return out
}
