package buffcloud

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestInputOfAnthropicKeepsOnlyTheNewTurn(t *testing.T) {
	body := `{"model":"claude-opus-5-5","system":"sys","messages":[
		{"role":"user","content":"first question"},
		{"role":"assistant","content":[{"type":"text","text":"first answer"}]},
		{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"file contents"}]},{"type":"text","text":"and now this"},{"type":"image","source":{}}]}
	]}`
	parts, cut := InputOf([]byte(body))
	if cut {
		t.Fatal("not cut")
	}
	want := []Part{
		{Kind: "tool_result", Role: "user", Text: "file contents"},
		{Kind: "text", Role: "user", Text: "and now this"},
		{Kind: "image", Role: "user"},
	}
	if len(parts) != len(want) {
		t.Fatalf("got %+v", parts)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("part %d: got %+v, want %+v", i, parts[i], want[i])
		}
	}
}

func TestInputOfChatAndResponses(t *testing.T) {
	chat, _ := InputOf([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"tool","content":"42"}]}`))
	if len(chat) != 1 || chat[0].Kind != "tool_result" || chat[0].Text != "42" {
		t.Fatalf("chat: %+v", chat)
	}
	responses, _ := InputOf([]byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]},{"type":"function_call","name":"shell","arguments":"{}"},{"type":"function_call_output","output":"done"},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`))
	if len(responses) != 2 || responses[0].Text != "done" || responses[1].Text != "next" {
		t.Fatalf("responses: %+v", responses)
	}
	plain, _ := InputOf([]byte(`{"input":"just text"}`))
	if len(plain) != 1 || plain[0].Text != "just text" {
		t.Fatalf("plain: %+v", plain)
	}
}

func TestInputOfClipsLongText(t *testing.T) {
	long := strings.Repeat("é", partLimit)
	parts, cut := InputOf([]byte(`{"messages":[{"role":"user","content":"` + long + `"}]}`))
	if !cut || len(parts[0].Text) > partLimit || !strings.HasPrefix(long, parts[0].Text) {
		t.Fatalf("cut=%v len=%d", cut, len(parts[0].Text))
	}
}

func TestOutputOfAnthropicStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`, `data: {"type":"message_start","message":{}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hel"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lo"}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","name":"Read"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"a\"}"}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")
	parts, _ := OutputOf([]byte(stream))
	want := []Part{
		{Kind: "thinking", Role: "assistant", Text: "hmm"},
		{Kind: "text", Role: "assistant", Text: "Hello"},
		{Kind: "tool_use", Role: "assistant", Name: "Read", Text: `{"path":"a"}`},
	}
	if len(parts) != len(want) {
		t.Fatalf("got %+v", parts)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("part %d: got %+v, want %+v", i, parts[i], want[i])
		}
	}
}

func TestOutputOfChatStreamAndJSON(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ng\",\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"ls\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n"
	parts, _ := OutputOf([]byte(stream))
	if len(parts) != 2 || parts[0].Text != "pong" || parts[1].Name != "ls" {
		t.Fatalf("chat stream: %+v", parts)
	}
	parts, _ = OutputOf([]byte(`{"content":[{"type":"text","text":"done"}]}`))
	if len(parts) != 1 || parts[0].Text != "done" {
		t.Fatalf("anthropic json: %+v", parts)
	}
	parts, _ = OutputOf([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	if len(parts) != 1 || parts[0].Text != "ok" {
		t.Fatalf("chat json: %+v", parts)
	}
}

func TestOutputOfResponsesStream(t *testing.T) {
	stream := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"reasoning\",\"summary\":[{\"text\":\"plan\"}]},{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]},{\"type\":\"function_call\",\"name\":\"shell\",\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}]}}\n\n"
	parts, _ := OutputOf([]byte(stream))
	if len(parts) != 3 || parts[0].Kind != "thinking" || parts[1].Text != "answer" || parts[2].Name != "shell" {
		t.Fatalf("responses: %+v", parts)
	}
}

func TestMessagesLogQueryNewestFirstWithFilters(t *testing.T) {
	l := OpenMessagesLog(t.TempDir(), nil)
	base := time.Date(2026, 10, 25, 23, 59, 0, 0, time.Local).UnixMilli()
	session := "s1"
	for i, text := range []string{"alpha", "beta", "gamma"} {
		row := MessageRow{At: base + int64(i)*60_000, TraceID: text, AccountID: "a.json", Input: []Part{{Kind: "text", Role: "user", Text: text}}}
		if i > 0 {
			row.SessionKey = &session
		}
		if err := l.Append(row); err != nil {
			t.Fatal(err)
		}
	}
	rows, more, err := l.Query(MessageQuery{})
	if err != nil || more || len(rows) != 3 || rows[0].TraceID != "gamma" || rows[2].TraceID != "alpha" {
		t.Fatalf("all: %v %v %+v", err, more, rows)
	}
	rows, more, _ = l.Query(MessageQuery{Limit: 1})
	if !more || len(rows) != 1 || rows[0].TraceID != "gamma" {
		t.Fatalf("limit: %v %+v", more, rows)
	}
	rows, _, _ = l.Query(MessageQuery{Before: rows[0].At, Limit: 1})
	if len(rows) != 1 || rows[0].TraceID != "beta" {
		t.Fatalf("before: %+v", rows)
	}
	rows, _, _ = l.Query(MessageQuery{Session: "s1", Text: "BET"})
	if len(rows) != 1 || rows[0].TraceID != "beta" {
		t.Fatalf("session and text: %+v", rows)
	}
	rows, _, _ = l.Query(MessageQuery{TraceID: "alpha"})
	if len(rows) != 1 {
		t.Fatalf("trace: %+v", rows)
	}
}

func TestMessagesLogPrunesOldDays(t *testing.T) {
	l := OpenMessagesLog(t.TempDir(), nil)
	l.keepDays = 2
	old := time.Now().AddDate(0, 0, -5)
	_ = l.Append(MessageRow{At: old.UnixMilli(), Input: []Part{{Kind: "text", Text: "old"}}})
	_ = l.Append(MessageRow{At: time.Now().UnixMilli(), Input: []Part{{Kind: "text", Text: "new"}}})
	if days := l.days(); len(days) != 1 || days[0] != time.Now().Format(dayLayout) {
		t.Fatalf("days: %v", days)
	}
}

func TestMiddlewareRecordsTheTurnWithItsAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	model := "glm-5.3"
	l := OpenMessagesLog(t.TempDir(), func(traceID string) (Row, bool) {
		if traceID != "trace-1" {
			return Row{}, false
		}
		return Row{AccountID: "opencode.json", Provider: "opencode-go", Model: &model}, true
	})
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(logging.WithRequestID(c.Request.Context(), "trace-1"))
	})
	r.Use(l.Middleware())
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		body := make([]byte, 64)
		n, _ := c.Request.Body.Read(body)
		if n == 0 {
			t.Error("the handler must still read the body")
		}
		c.JSON(http.StatusOK, gin.H{"choices": []gin.H{{"message": gin.H{"content": "pong"}}}})
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.3","messages":[{"role":"user","content":"ping"}]}`))
	req.Header.Set("X-Claude-Code-Session-Id", "sess")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("client reply: %s", w.Body.String())
	}
	var rows []MessageRow
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, _ = l.Query(MessageQuery{})
		if len(rows) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatal("no row written")
	}
	row := rows[0]
	if row.AccountID != "opencode.json" || row.Provider != "opencode-go" || *row.Model != "glm-5.3" || *row.SessionKey != "sess" || row.Status != 200 {
		t.Fatalf("row: %+v", row)
	}
	if len(row.Input) != 1 || row.Input[0].Text != "ping" || len(row.Output) != 1 || row.Output[0].Text != "pong" {
		t.Fatalf("parts: %+v / %+v", row.Input, row.Output)
	}
}
