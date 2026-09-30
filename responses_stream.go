package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

type responseTool struct {
	index     int
	arguments strings.Builder
}

// Each upstream attempt owns its own decoder, so retries cannot reuse tool
// indices, accumulated arguments, response IDs, or reasoning from a failed key.
type responseStream struct {
	model    string
	response gjson.Result
	seen     bool
	finished bool
	roleSent bool
	legacy   bool
	tools    map[int64]*responseTool
	texts    map[string]*strings.Builder
}

func (s *responseStream) terminal() bool { return s.finished }

func (s *responseStream) eofError() error {
	if s.finished || (s.legacy && !s.seen) {
		return nil
	}
	return io.ErrUnexpectedEOF
}

func (s *responseStream) convert(payload []byte) ([][]byte, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	event := gjson.ParseBytes(payload)
	kind := event.Get("type").String()
	if err := responseEventFailure(event); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(kind, "response.") {
		// Keep normalization for mirrors that return legacy chat chunks.
		if event.Get("choices").IsArray() && !s.seen {
			s.legacy = true
			fixed, _ := mapReasoningBody(payload)
			return [][]byte{fixed}, nil
		}
		return nil, nil
	}
	s.seen = true
	if s.tools == nil {
		s.tools = make(map[int64]*responseTool)
		s.texts = make(map[string]*strings.Builder)
	}
	switch kind {
	case "response.created", "response.in_progress":
		s.response = event.Get("response")
		return nil, nil
	case "response.output_text.delta", "response.output_text.done":
		return s.text(event, "content", "content_index", strings.HasSuffix(kind, ".done")), nil
	case "response.refusal.delta", "response.refusal.done":
		return s.text(event, "refusal", "content_index", strings.HasSuffix(kind, ".done")), nil
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		return s.text(event, "reasoning_content", "summary_index", strings.HasSuffix(kind, ".done")), nil
	case "response.reasoning_text.delta", "response.reasoning_text.done":
		return s.text(event, "reasoning_content", "content_index", strings.HasSuffix(kind, ".done")), nil
	case "response.output_item.added":
		if event.Get("item.type").String() == "function_call" {
			return s.tool(event.Get("output_index").Int(), event.Get("item")), nil
		}
	case "response.output_item.done":
		return s.outputItem(event.Get("output_index").Int(), event.Get("item")), nil
	case "response.function_call_arguments.delta", "response.function_call_arguments.done":
		tool := s.tools[event.Get("output_index").Int()]
		if tool == nil {
			return nil, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: function arguments arrived before their tool call"}
		}
		arguments := event.Get("delta").String()
		if strings.HasSuffix(kind, ".done") {
			arguments = unsentSuffix(event.Get("arguments").String(), tool.arguments.String())
		}
		tool.arguments.WriteString(arguments)
		if arguments != "" {
			return [][]byte{s.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index": tool.index, "function": map[string]any{"arguments": arguments},
			}}}, "")}, nil
		}
	case "response.completed", "response.incomplete":
		s.response = event.Get("response")
		if err := validateResponseResult(s.response); err != nil {
			return nil, err
		}
		var chunks [][]byte
		// Terminal items are full snapshots. Reconcile any missing deltas
		// without replaying text, reasoning or tool arguments already sent.
		for index, item := range s.response.Get("output").Array() {
			chunks = append(chunks, s.outputItem(int64(index), item)...)
		}
		s.finished = true
		chunks = append(chunks, s.chunk(map[string]any{}, responseFinishReason(s.response, len(s.tools) > 0)))
		if usage := responseUsage(s.response.Get("usage")); usage != nil {
			out := responseEnvelope(s.response, s.model, true)
			out["choices"] = []any{}
			out["usage"] = usage
			body, _ := json.Marshal(out)
			chunks = append(chunks, body)
		}
		return chunks, nil
	}
	return nil, nil
}

// A final snapshot is a full value, not another delta. Only append the unseen
// suffix; replaying the whole snapshot duplicates text or function arguments.
func unsentSuffix(full, sent string) string {
	if strings.HasPrefix(full, sent) {
		return full[len(sent):]
	}
	return ""
}

func (s *responseStream) text(event gjson.Result, field, indexField string, done bool) [][]byte {
	text := event.Get("delta").String()
	if done {
		text = event.Get("text").String()
		if field == "refusal" {
			text = event.Get("refusal").String()
		}
	}
	return s.textValue(event.Get("output_index").Int(), event.Get(indexField).Int(), field, indexField, text, done)
}

func (s *responseStream) textValue(outputIndex, partIndex int64, field, indexField, text string, done bool) [][]byte {
	key := fmt.Sprintf("%s/%s/%d/%d", field, indexField, outputIndex, partIndex)
	sent := s.texts[key]
	if sent == nil {
		sent = &strings.Builder{}
		s.texts[key] = sent
	}
	if done {
		text = unsentSuffix(text, sent.String())
	}
	sent.WriteString(text)
	if text == "" {
		return nil
	}
	return [][]byte{s.chunk(map[string]any{field: text}, "")}
}

func (s *responseStream) outputItem(outputIndex int64, item gjson.Result) [][]byte {
	if item.Get("type").String() == "function_call" {
		return s.tool(outputIndex, item)
	}
	var chunks [][]byte
	for _, field := range []string{"summary", "content"} {
		for index, part := range item.Get(field).Array() {
			deltaField, indexField, text := "", "content_index", part.Get("text").String()
			switch part.Get("type").String() {
			case "output_text":
				deltaField = "content"
			case "summary_text":
				deltaField, indexField = "reasoning_content", "summary_index"
			case "reasoning_text":
				deltaField = "reasoning_content"
			case "refusal":
				deltaField, text = "refusal", part.Get("refusal").String()
			}
			if deltaField != "" {
				chunks = append(chunks, s.textValue(outputIndex, int64(index), deltaField, indexField, text, true)...)
			}
		}
	}
	return chunks
}

func (s *responseStream) tool(outputIndex int64, item gjson.Result) [][]byte {
	arguments := item.Get("arguments").String()
	tool := s.tools[outputIndex]
	if tool == nil {
		tool = &responseTool{index: len(s.tools)}
		tool.arguments.WriteString(arguments)
		s.tools[outputIndex] = tool
		return [][]byte{s.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": tool.index, "id": item.Get("call_id").String(), "type": "function",
			"function": map[string]any{"name": item.Get("name").String(), "arguments": arguments},
		}}}, "")}
	}
	arguments = unsentSuffix(arguments, tool.arguments.String())
	tool.arguments.WriteString(arguments)
	if arguments == "" {
		return nil
	}
	return [][]byte{s.chunk(map[string]any{"tool_calls": []any{map[string]any{
		"index": tool.index, "function": map[string]any{"arguments": arguments},
	}}}, "")}
}

func (s *responseStream) chunk(delta map[string]any, finish string) []byte {
	if !s.roleSent {
		delta["role"] = "assistant"
		s.roleSent = true
	}
	var reason any
	if finish != "" {
		reason = finish
	}
	out := responseEnvelope(s.response, s.model, true)
	out["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": reason}}
	body, _ := json.Marshal(out)
	return body
}
