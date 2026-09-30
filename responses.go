package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// chatToResponses keeps the host-facing executor format as Chat Completions
// while using the Responses protocol on the upstream wire. Only fields that
// Responses accepts are copied; chat-only stream_options must never leak.
func chatToResponses(body []byte, stream bool) ([]byte, error) {
	root := gjson.ParseBytes(body)
	if !gjson.ValidBytes(body) || !root.IsObject() || !root.Get("messages").IsArray() {
		return nil, statusError{statusCode: http.StatusBadRequest, msg: "commandcode executor: expected a Chat Completions request with messages"}
	}
	input := make([]any, 0)
	for _, message := range root.Get("messages").Array() {
		role := message.Get("role").String()
		content, err := responsesContent(message.Get("content"), role == "assistant")
		if err != nil {
			return nil, statusError{statusCode: http.StatusBadRequest, msg: err.Error()}
		}
		if role == "tool" {
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": message.Get("tool_call_id").String(), "output": content,
			})
			continue
		}
		if role != "system" && role != "developer" && role != "user" && role != "assistant" {
			return nil, statusError{statusCode: http.StatusBadRequest, msg: fmt.Sprintf("commandcode executor: unsupported message role %q", role)}
		}
		toolCalls := message.Get("tool_calls").Array()
		if role != "assistant" || len(toolCalls) == 0 || hasMessageContent(content) {
			input = append(input, map[string]any{"type": "message", "role": role, "content": content})
		}
		for _, call := range toolCalls {
			if call.Get("type").String() != "function" {
				return nil, statusError{statusCode: http.StatusBadRequest, msg: "commandcode executor: only function tool calls are supported"}
			}
			input = append(input, map[string]any{
				"type": "function_call", "call_id": call.Get("id").String(),
				"name": call.Get("function.name").String(), "arguments": call.Get("function.arguments").String(),
			})
		}
	}
	out := map[string]any{"input": input, "stream": stream}
	for _, field := range []string{
		"model", "temperature", "top_p", "parallel_tool_calls", "metadata", "store", "user", "service_tier",
		"reasoning", "text", "include", "instructions", "previous_response_id", "truncation",
		"prompt_cache_key", "prompt_cache_retention", "safety_identifier",
	} {
		if value := root.Get(field); value.Exists() {
			out[field] = value.Value()
		}
	}
	for _, field := range []string{"max_completion_tokens", "max_tokens"} {
		if value := root.Get(field); value.Exists() {
			out["max_output_tokens"] = value.Value()
			break
		}
	}
	if effort := root.Get("reasoning_effort"); effort.Exists() {
		reasoning, _ := out["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		reasoning["effort"] = effort.Value()
		out["reasoning"] = reasoning
	}
	if format := root.Get("response_format"); format.IsObject() {
		text, _ := out["text"].(map[string]any)
		if text == nil {
			text = map[string]any{}
		}
		if format.Get("type").String() == "json_schema" {
			flat, _ := format.Get("json_schema").Value().(map[string]any)
			if flat == nil {
				flat = map[string]any{}
			}
			flat["type"] = "json_schema"
			text["format"] = flat
		} else {
			text["format"] = format.Value()
		}
		out["text"] = text
	}
	if tools := root.Get("tools"); tools.IsArray() {
		flat := make([]any, 0, len(tools.Array()))
		for _, tool := range tools.Array() {
			if tool.Get("type").String() != "function" || !tool.Get("function").IsObject() {
				return nil, statusError{statusCode: http.StatusBadRequest, msg: "commandcode executor: only function tools are supported"}
			}
			function := tool.Get("function").Value().(map[string]any)
			function["type"] = "function"
			// Responses defaults to strict schemas; preserve Chat Completions'
			// non-strict default unless the client explicitly opts in.
			if _, exists := function["strict"]; !exists {
				function["strict"] = false
			}
			flat = append(flat, function)
		}
		out["tools"] = flat
	}
	if choice := root.Get("tool_choice"); choice.Exists() {
		if choice.IsObject() && choice.Get("type").String() == "function" {
			out["tool_choice"] = map[string]any{"type": "function", "name": choice.Get("function.name").String()}
		} else {
			out["tool_choice"] = choice.Value()
		}
	}
	return json.Marshal(out)
}

func hasMessageContent(content any) bool {
	switch value := content.(type) {
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	}
	return false
}

// Responses accepts simple string content as well as typed multimodal parts.
func responsesContent(content gjson.Result, assistant bool) (any, error) {
	if content.Type == gjson.Null {
		return "", nil
	}
	if content.Type == gjson.String {
		return content.String(), nil
	}
	if !content.IsArray() {
		return nil, fmt.Errorf("commandcode executor: unsupported message content")
	}
	parts := make([]any, 0, len(content.Array()))
	for _, part := range content.Array() {
		switch part.Get("type").String() {
		case "text":
			kind := "input_text"
			if assistant {
				kind = "output_text"
			}
			parts = append(parts, map[string]any{"type": kind, "text": part.Get("text").String()})
		case "image_url":
			image := map[string]any{"type": "input_image", "image_url": part.Get("image_url.url").String()}
			if detail := part.Get("image_url.detail"); detail.Exists() {
				image["detail"] = detail.Value()
			}
			parts = append(parts, image)
		case "file":
			file := map[string]any{"type": "input_file"}
			for _, field := range []string{"file_id", "file_data", "filename"} {
				if value := part.Get("file." + field); value.Exists() {
					file[field] = value.Value()
				}
			}
			parts = append(parts, file)
		case "refusal":
			parts = append(parts, map[string]any{"type": "refusal", "refusal": part.Get("refusal").String()})
		default:
			return nil, fmt.Errorf("commandcode executor: unsupported content part %q", part.Get("type").String())
		}
	}
	return parts, nil
}

func responsesToChat(body []byte, model string) ([]byte, error) {
	root := gjson.ParseBytes(body)
	if !gjson.ValidBytes(body) || !root.IsObject() {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: invalid Responses JSON"}
	}
	if err := validateResponseResult(root); err != nil {
		return nil, err
	}
	message := map[string]any{"role": "assistant", "content": nil}
	var text, reasoning, refusal strings.Builder
	tools := make([]any, 0)
	for _, item := range root.Get("output").Array() {
		switch item.Get("type").String() {
		case "message":
			for _, part := range item.Get("content").Array() {
				switch part.Get("type").String() {
				case "output_text":
					text.WriteString(part.Get("text").String())
				case "refusal":
					refusal.WriteString(part.Get("refusal").String())
				}
			}
		case "reasoning":
			for _, field := range []string{"summary", "content"} {
				for _, part := range item.Get(field).Array() {
					reasoning.WriteString(part.Get("text").String())
				}
			}
		case "function_call":
			tools = append(tools, map[string]any{
				"id": item.Get("call_id").String(), "type": "function",
				"function": map[string]any{"name": item.Get("name").String(), "arguments": item.Get("arguments").String()},
			})
		}
	}
	if text.Len() > 0 {
		message["content"] = text.String()
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if refusal.Len() > 0 {
		message["refusal"] = refusal.String()
	}
	if len(tools) > 0 {
		message["tool_calls"] = tools
	}
	out := responseEnvelope(root, model, false)
	out["choices"] = []any{map[string]any{"index": 0, "message": message, "finish_reason": responseFinishReason(root, len(tools) > 0)}}
	if usage := responseUsage(root.Get("usage")); usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

func validateResponseResult(response gjson.Result) error {
	if err := responseFailure(response); err != nil {
		return err
	}
	if !response.IsObject() || !response.Get("output").IsArray() {
		return statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: Responses output is missing"}
	}
	status := response.Get("status").String()
	if status != "completed" && status != "incomplete" {
		return statusError{statusCode: http.StatusBadGateway, msg: fmt.Sprintf("commandcode executor: unexpected Responses status %q", status)}
	}
	return nil
}

func responseEnvelope(response gjson.Result, model string, stream bool) map[string]any {
	if value := response.Get("model").String(); value != "" {
		model = value
	}
	object := "chat.completion"
	if stream {
		object += ".chunk"
	}
	return map[string]any{"id": response.Get("id").String(), "object": object, "created": response.Get("created_at").Int(), "model": model}
}

func responseFinishReason(response gjson.Result, tools bool) string {
	if response.Get("status").String() == "incomplete" {
		switch response.Get("incomplete_details.reason").String() {
		case "max_output_tokens":
			return "length"
		case "content_filter":
			return "content_filter"
		}
	}
	if tools {
		return "tool_calls"
	}
	return "stop"
}

func responseUsage(usage gjson.Result) map[string]any {
	if !usage.IsObject() {
		return nil
	}
	out := map[string]any{
		"prompt_tokens": usage.Get("input_tokens").Int(), "completion_tokens": usage.Get("output_tokens").Int(),
		"total_tokens": usage.Get("total_tokens").Int(),
	}
	for from, to := range map[string]string{"input_tokens_details": "prompt_tokens_details", "output_tokens_details": "completion_tokens_details"} {
		if details := usage.Get(from); details.IsObject() {
			fields := details.Value().(map[string]any)
			if value, exists := fields["cache_write_tokens"]; exists && from == "input_tokens_details" {
				fields["cached_creation_tokens"] = value
				delete(fields, "cache_write_tokens")
			}
			out[to] = fields
		}
	}
	return out
}

// Responses may report a failure inside a 2xx body or an SSE event. Turn it
// into the same status error used by HTTP failover instead of treating it as
// a successful empty completion.
func responseFailure(response gjson.Result) error {
	failure := response.Get("error")
	if failure.Type == gjson.Null && response.Get("status").String() != "failed" {
		return nil
	}
	if failure.Type == gjson.Null {
		failure = gjson.Parse(`{"message":"upstream response failed"}`)
	}
	status := http.StatusBadGateway
	code := failure.Get("code").String()
	if code == "" {
		code = failure.Get("type").String()
	}
	switch code {
	case "invalid_request_error", "invalid_request", "invalid_prompt", "context_length_exceeded":
		status = http.StatusBadRequest
	case "permission_denied", "forbidden":
		status = http.StatusForbidden
	case "authentication_error", "invalid_api_key":
		status = http.StatusUnauthorized
	case "rate_limit_exceeded", "insufficient_quota":
		status = http.StatusTooManyRequests
	}
	body, _ := json.Marshal(map[string]any{"error": failure.Value()})
	return statusError{statusCode: status, body: body}
}
