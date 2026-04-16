package apiv1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAskRequest_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := AskRequest{
		Query:          "what did my namespace cost yesterday?",
		Model:          "qwen2.5:7b-instruct",
		Stream:         true,
		ConversationID: "f47ac10b-58cc-4372-a567-0e02b2c3d479",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out AskRequest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\nin:  %+v\nout: %+v", in, out)
	}
}

func TestAskRequest_OmitsZeroValues(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(AskRequest{Query: "hi"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	// Required field present.
	if !strings.Contains(got, `"query":"hi"`) {
		t.Errorf("missing query: %s", got)
	}
	// Optional fields omitted when empty.
	for _, unwanted := range []string{"model", "stream", "conversation_id"} {
		if strings.Contains(got, `"`+unwanted+`"`) {
			t.Errorf("unexpected field %q in %s", unwanted, got)
		}
	}
}

func TestAskResponse_MarshalShape(t *testing.T) {
	t.Parallel()
	resp := AskResponse{
		RequestID: "11111111-1111-1111-1111-111111111111",
		Model:     "qwen2.5:7b-instruct",
		Query:     "q",
		Answer:    "a",
		ToolCalls: []ToolCall{{
			Name:       "opencost.allocation",
			Args:       map[string]any{"window": "24h"},
			DurationMS: 42,
		}},
		Usage:     Usage{PromptTokens: 10, CompletionTokens: 5},
		LatencyMS: 123,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Re-decode into a generic map so we can assert on the wire shape
	// without tying the test to Go field order.
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"request_id", "model", "query", "answer", "tool_calls", "usage", "latency_ms"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing key %q in %s", k, b)
		}
	}
}

func TestAskResponse_OmitsEmptyToolCalls(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(AskResponse{RequestID: "r"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "tool_calls") {
		t.Errorf("tool_calls should be omitted when empty: %s", b)
	}
}

func TestProblem_JSONShape(t *testing.T) {
	t.Parallel()
	p := Problem{
		Type:      "https://opencost.io/problems/unauthenticated",
		Title:     "Unauthenticated",
		Status:    401,
		Detail:    "bearer token missing or invalid",
		Instance:  "/v1/ask#req_abc",
		RequestID: "req_abc",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"type", "title", "status", "detail", "instance", "request_id"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if got["status"].(float64) != 401 {
		t.Errorf("status wrong: %v", got["status"])
	}
}

func TestProblem_OmitsOptionalsWhenEmpty(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Problem{Title: "Bad Request", Status: 400})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, unwanted := range []string{"type", "detail", "instance", "request_id"} {
		if strings.Contains(got, `"`+unwanted+`"`) {
			t.Errorf("unexpected field %q in %s", unwanted, got)
		}
	}
}

func TestProblemContentType(t *testing.T) {
	t.Parallel()
	if ProblemContentType != "application/problem+json" {
		t.Fatalf("RFC 7807 content type regressed: got %q", ProblemContentType)
	}
}
