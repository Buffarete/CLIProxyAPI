package buffcloud

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// Part is one piece of a message: text, a tool call, a tool's result, the
// model's thinking or an image (named, not kept).
type Part struct {
	Kind string `json:"kind"`
	Role string `json:"role,omitempty"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
}

const (
	// partLimit caps one part's text; a request keeps at most partsLimit parts a side.
	partLimit  = 16 * 1024
	partsLimit = 64
)

func clip(s string) (string, bool) {
	if len(s) <= partLimit {
		return s, false
	}
	cut := partLimit
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

type parts struct {
	list      []Part
	truncated bool
}

func (p *parts) add(part Part) {
	if part.Text == "" && part.Kind != "image" && part.Name == "" {
		return
	}
	if len(p.list) >= partsLimit {
		p.truncated = true
		return
	}
	var cut bool
	part.Text, cut = clip(part.Text)
	p.truncated = p.truncated || cut
	p.list = append(p.list, part)
}

// InputOf returns what the client added since the model's last turn: the new
// user message, or the tool results Claude Code and Codex send back. The rest of
// the conversation is already in earlier rows. It reads Anthropic Messages,
// OpenAI Chat Completions and OpenAI Responses bodies.
func InputOf(body []byte) ([]Part, bool) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
		Input    json.RawMessage   `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil, false
	}
	var out parts
	if len(req.Messages) > 0 {
		for _, raw := range afterLastModelTurn(req.Messages, func(m map[string]json.RawMessage) bool {
			return str(m["role"]) == "assistant"
		}) {
			chatMessageParts(raw, &out)
		}
		return out.list, out.truncated
	}
	if len(req.Input) > 0 {
		var text string
		if json.Unmarshal(req.Input, &text) == nil {
			out.add(Part{Kind: "text", Role: "user", Text: text})
			return out.list, out.truncated
		}
		var items []json.RawMessage
		if json.Unmarshal(req.Input, &items) == nil {
			for _, raw := range afterLastModelTurn(items, func(m map[string]json.RawMessage) bool {
				t := str(m["type"])
				return str(m["role"]) == "assistant" || t == "function_call" || t == "custom_tool_call" || t == "reasoning"
			}) {
				responsesItemParts(raw, &out)
			}
		}
	}
	return out.list, out.truncated
}

func afterLastModelTurn(items []json.RawMessage, isModel func(map[string]json.RawMessage) bool) []json.RawMessage {
	start := 0
	for i, raw := range items {
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) == nil && isModel(m) {
			start = i + 1
		}
	}
	return items[start:]
}

func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// Anthropic and Chat Completions messages: content is a string or a list of blocks.
func chatMessageParts(raw json.RawMessage, out *parts) {
	var m struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if m.Role == "system" || m.Role == "developer" {
		return
	}
	kind := "text"
	if m.Role == "tool" {
		kind = "tool_result"
	}
	var text string
	if json.Unmarshal(m.Content, &text) == nil {
		out.add(Part{Kind: kind, Role: m.Role, Text: text})
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		blockParts(b, m.Role, out)
	}
}

func blockParts(raw json.RawMessage, role string, out *parts) {
	var b struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Content  json.RawMessage `json:"content"`
		Thinking string          `json:"thinking"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return
	}
	switch b.Type {
	case "text", "input_text", "output_text":
		out.add(Part{Kind: "text", Role: role, Text: b.Text})
	case "image", "image_url", "input_image":
		out.add(Part{Kind: "image", Role: role})
	case "tool_use":
		out.add(Part{Kind: "tool_use", Role: role, Name: b.Name, Text: string(b.Input)})
	case "tool_result":
		out.add(Part{Kind: "tool_result", Role: role, Text: contentText(b.Content)})
	case "thinking":
		out.add(Part{Kind: "thinking", Role: role, Text: b.Thinking})
	}
}

// A tool result's content is a string or a list of text and image blocks.
func contentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Text == "" {
			if b.Type == "image" || b.Type == "input_image" {
				b.Text = "[image]"
			} else {
				continue
			}
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

func responsesItemParts(raw json.RawMessage, out *parts) {
	var it struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Output    json.RawMessage `json:"output"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		Input     string          `json:"input"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return
	}
	switch it.Type {
	case "", "message":
		if it.Role == "system" || it.Role == "developer" {
			return
		}
		var text string
		if json.Unmarshal(it.Content, &text) == nil {
			out.add(Part{Kind: "text", Role: it.Role, Text: text})
			return
		}
		var blocks []json.RawMessage
		if json.Unmarshal(it.Content, &blocks) == nil {
			for _, b := range blocks {
				blockParts(b, it.Role, out)
			}
		}
	case "function_call_output", "custom_tool_call_output":
		out.add(Part{Kind: "tool_result", Role: "tool", Text: contentText(it.Output)})
	case "function_call":
		out.add(Part{Kind: "tool_use", Role: "assistant", Name: it.Name, Text: it.Arguments})
	case "custom_tool_call":
		out.add(Part{Kind: "tool_use", Role: "assistant", Name: it.Name, Text: it.Input})
	}
}

// OutputOf returns the model's reply from a response body, streamed (SSE) or
// not, in any of the three formats InputOf reads.
func OutputOf(body []byte) ([]Part, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] == '{' {
		var out parts
		jsonReplyParts(trimmed, &out)
		return out.list, out.truncated
	}
	return sseReply(body)
}

func jsonReplyParts(body []byte, out *parts) {
	var r struct {
		Content []json.RawMessage `json:"content"`
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
		Output   []json.RawMessage `json:"output"`
		Response *struct {
			Output []json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &r) != nil {
		return
	}
	for _, b := range r.Content {
		blockParts(b, "assistant", out)
	}
	for _, c := range r.Choices {
		chatReply(c.Message, out)
	}
	items := r.Output
	if r.Response != nil {
		items = append(items, r.Response.Output...)
	}
	for _, it := range items {
		responsesOutputItem(it, out)
	}
}

func chatReply(raw json.RawMessage, out *parts) {
	var m struct {
		Content   string `json:"content"`
		ToolCalls []struct {
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	out.add(Part{Kind: "text", Role: "assistant", Text: m.Content})
	for _, t := range m.ToolCalls {
		out.add(Part{Kind: "tool_use", Role: "assistant", Name: t.Function.Name, Text: t.Function.Arguments})
	}
}

func responsesOutputItem(raw json.RawMessage, out *parts) {
	var it struct {
		Type    string `json:"type"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return
	}
	if it.Type == "reasoning" {
		for _, s := range it.Summary {
			out.add(Part{Kind: "thinking", Role: "assistant", Text: s.Text})
		}
		return
	}
	if it.Type == "message" {
		var m struct {
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) == nil {
			for _, b := range m.Content {
				blockParts(b, "assistant", out)
			}
		}
		return
	}
	responsesItemParts(raw, out)
}

// sseReply rebuilds the reply from a stream. Anthropic sends content blocks and
// their deltas; Chat Completions sends choice deltas; Responses ends with
// response.completed, which holds the whole output.
func sseReply(body []byte) ([]Part, bool) {
	type block struct {
		kind, name string
		text       strings.Builder
	}
	var blocks []*block
	byIndex := map[int]*block{}
	var chatText strings.Builder
	chatTools := map[int]*block{}
	var chatOrder []int
	var completed []byte

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[5:])
		if len(data) == 0 || data[0] != '{' {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock *struct {
				Type string `json:"type"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta   json.RawMessage `json:"delta"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int `json:"index"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "content_block_start" && ev.ContentBlock != nil:
			kind := map[string]string{"text": "text", "tool_use": "tool_use", "server_tool_use": "tool_use", "thinking": "thinking"}[ev.ContentBlock.Type]
			if kind == "" {
				continue
			}
			b := &block{kind: kind, name: ev.ContentBlock.Name}
			b.text.WriteString(ev.ContentBlock.Text)
			blocks = append(blocks, b)
			byIndex[ev.Index] = b
		case ev.Type == "content_block_delta":
			b := byIndex[ev.Index]
			if b == nil || b.text.Len() > partLimit {
				continue
			}
			var d struct {
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
			}
			if json.Unmarshal(ev.Delta, &d) == nil {
				b.text.WriteString(d.Text + d.PartialJSON + d.Thinking)
			}
		case ev.Type == "response.completed" || ev.Type == "response.incomplete" || ev.Type == "response.failed":
			completed = data
		case len(ev.Choices) > 0:
			for _, c := range ev.Choices {
				if chatText.Len() <= partLimit {
					chatText.WriteString(c.Delta.Content)
				}
				for _, t := range c.Delta.ToolCalls {
					b := chatTools[t.Index]
					if b == nil {
						b = &block{kind: "tool_use"}
						chatTools[t.Index] = b
						chatOrder = append(chatOrder, t.Index)
					}
					if t.Function.Name != "" {
						b.name = t.Function.Name
					}
					if b.text.Len() <= partLimit {
						b.text.WriteString(t.Function.Arguments)
					}
				}
			}
		}
	}

	var out parts
	if completed != nil {
		jsonReplyParts(completed, &out)
		return out.list, out.truncated
	}
	for _, b := range blocks {
		out.add(Part{Kind: b.kind, Role: "assistant", Name: b.name, Text: b.text.String()})
	}
	out.add(Part{Kind: "text", Role: "assistant", Text: chatText.String()})
	for _, i := range chatOrder {
		b := chatTools[i]
		out.add(Part{Kind: b.kind, Role: "assistant", Name: b.name, Text: b.text.String()})
	}
	return out.list, out.truncated
}
