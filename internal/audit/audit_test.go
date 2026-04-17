package audit

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

func TestCallerIdentity_StableAndTruncated(t *testing.T) {
	t.Parallel()
	got := CallerIdentity("s3cret-token")
	if len(got) != callerIdentityPrefixHex {
		t.Fatalf("len = %d, want %d", len(got), callerIdentityPrefixHex)
	}
	// Identity must be stable across calls.
	if got != CallerIdentity("s3cret-token") {
		t.Errorf("identity not stable")
	}
	// Distinct tokens produce distinct identities (SHA-256 collision
	// resistance makes this a rock-solid assumption).
	if got == CallerIdentity("other-token") {
		t.Errorf("collision on distinct tokens")
	}
	// Empty token is labelled anonymous, not silently hashed to a
	// fixed value that looks like a real caller.
	if CallerIdentity("") != "anonymous" {
		t.Errorf("empty token identity = %q", CallerIdentity(""))
	}
}

func TestLogger_EmitsStableShape(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewLogger(&buf, false).WithClock(fixedClock(time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC)))

	err := l.Log(Event{
		RequestID:        "rid-1",
		CallerIdentity:   "cafebabe",
		Model:            "qwen2.5:7b-instruct",
		PromptTokens:     100,
		CompletionTokens: 25,
		ToolCalls: []ToolCall{
			{Name: "opencost.allocation", DurationMS: 42},
		},
		LatencyMS: 500,
		Status:    200,
		Outcome:   "ok",
		// Query/Answer intentionally left empty here; the opt-in path
		// is covered below.
	})
	if err != nil {
		t.Fatalf("log: %v", err)
	}

	line := strings.TrimSpace(buf.String())
	var got Event
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("unmarshal: %v, line=%s", err, line)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("timestamp not populated")
	}
	if got.RequestID != "rid-1" {
		t.Errorf("request_id = %q", got.RequestID)
	}
	if got.CallerIdentity != "cafebabe" {
		t.Errorf("caller_identity = %q", got.CallerIdentity)
	}
	if got.Model != "qwen2.5:7b-instruct" {
		t.Errorf("model = %q", got.Model)
	}
	if got.PromptTokens != 100 || got.CompletionTokens != 25 {
		t.Errorf("tokens = %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "opencost.allocation" || got.ToolCalls[0].DurationMS != 42 {
		t.Errorf("tool_calls = %+v", got.ToolCalls)
	}
	if got.LatencyMS != 500 {
		t.Errorf("latency_ms = %d", got.LatencyMS)
	}
	if got.Status != 200 || got.Outcome != "ok" {
		t.Errorf("status/outcome = %d/%q", got.Status, got.Outcome)
	}
	if got.Query != "" || got.Answer != "" {
		t.Errorf("query/answer leaked when logQuery=false: q=%q a=%q", got.Query, got.Answer)
	}
}

func TestLogger_NeverLogsQueryWhenDisabled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewLogger(&buf, false)

	err := l.Log(Event{
		RequestID:      "rid-2",
		CallerIdentity: "abcd0123",
		Model:          "m",
		Status:         200,
		Outcome:        "ok",
		Query:          "top secret query about cluster cost",
		Answer:         "very sensitive answer with dollars",
	})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if strings.Contains(buf.String(), "top secret query") {
		t.Fatalf("query leaked to audit log: %s", buf.String())
	}
	if strings.Contains(buf.String(), "very sensitive answer") {
		t.Fatalf("answer leaked to audit log: %s", buf.String())
	}
	// Explicit: the JSON keys must also be absent, not just empty
	// strings, so grepping for "query": catches regressions.
	if strings.Contains(buf.String(), `"query":`) {
		t.Errorf("query key present when disabled: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"answer":`) {
		t.Errorf("answer key present when disabled: %s", buf.String())
	}
}

func TestLogger_IncludesQueryWhenEnabled(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewLogger(&buf, true)

	err := l.Log(Event{
		RequestID:      "rid-3",
		CallerIdentity: "abcd0123",
		Model:          "m",
		Status:         200,
		Outcome:        "ok",
		Query:          "what was yesterday's cost",
		Answer:         "it was forty two dollars",
	})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "what was yesterday's cost") {
		t.Errorf("query missing when enabled: %s", out)
	}
	if !strings.Contains(out, "it was forty two dollars") {
		t.Errorf("answer missing when enabled: %s", out)
	}
}

func TestLogger_LogQueryEnabled(t *testing.T) {
	t.Parallel()
	if !NewLogger(nil, true).LogQueryEnabled() {
		t.Errorf("enabled logger should report true")
	}
	if NewLogger(nil, false).LogQueryEnabled() {
		t.Errorf("disabled logger should report false")
	}
}

func TestLogger_ConcurrentWritesAreLineAtomic(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	l := NewLogger(&buf, false)

	var wg sync.WaitGroup
	const workers = 8
	const perWorker = 200
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				_ = l.Log(Event{
					RequestID:      "r",
					CallerIdentity: "c",
					Model:          "m",
					Status:         200,
					Outcome:        "ok",
				})
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != workers*perWorker {
		t.Fatalf("line count = %d, want %d", len(lines), workers*perWorker)
	}
	for _, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("garbled line %q: %v", line, err)
		}
	}
}
