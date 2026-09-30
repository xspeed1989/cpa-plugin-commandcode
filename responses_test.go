package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
)

const responsesFixture = `{"id":"resp_test","object":"response","created_at":123,"status":"completed","model":"deepseek/deepseek-v4.1-flash","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"think"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`

type responsesHTTPClient struct {
	requests []pluginapi.HTTPRequest
	bodies   [][]byte
	streams  [][]pluginapi.HTTPStreamChunk
}

func (c *responsesHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	index := len(c.requests)
	c.requests = append(c.requests, req)
	body := []byte(responsesFixture)
	if index < len(c.bodies) {
		body = c.bodies[index]
	}
	return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"X-Upstream": {"kept"}}, Body: body}, nil
}

func (c *responsesHTTPClient) DoStream(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	index := len(c.requests)
	c.requests = append(c.requests, req)
	input := []pluginapi.HTTPStreamChunk{{Payload: []byte(sseEvent(`{"type":"response.completed","response":` + responsesFixture + `}`))}}
	if index < len(c.streams) {
		input = c.streams[index]
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, len(input))
	for _, chunk := range input {
		chunks <- chunk
	}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: http.StatusOK, Chunks: chunks}, nil
}

func sseEvent(body string) string { return "data: " + body + "\n\n" }

func assertJSONFields(t *testing.T, body []byte, fields map[string]string) {
	t.Helper()
	if !gjson.ValidBytes(body) {
		t.Fatalf("invalid JSON: %s", body)
	}
	for path, want := range fields {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Errorf("%s=%q, want %q; body=%s", path, got, want, body)
		}
	}
}

func TestExecutorUsesResponsesProtocol(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			client := &responsesHTTPClient{}
			e := NewExecutor(parseConfig([]byte("api_key: test-key\nbase_url: https://mirror.example/provider/v1/\n")), nil)
			req := pluginapi.ExecutorRequest{
				Model: "deepseek-flash", HTTPClient: client,
				Payload: []byte(`{"model":"deepseek-flash","stream":true,"messages":[{"role":"user","content":"hello"}],"max_tokens":32,"max_completion_tokens":64,"temperature":0.5,"top_p":0.8,"reasoning_effort":"high","stream_options":{"include_usage":true},"n":1,"stop":["stop"]}`),
			}
			if stream {
				resp, err := e.ExecuteStream(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				for chunk := range resp.Chunks {
					if chunk.Err != nil {
						t.Fatal(chunk.Err)
					}
				}
			} else {
				resp, err := e.Execute(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				assertJSONFields(t, resp.Payload, map[string]string{
					"object": "chat.completion", "id": "resp_test", "created": "123",
					"choices.0.message.content": "answer", "choices.0.message.reasoning_content": "think",
					"choices.0.finish_reason": "stop", "usage.prompt_tokens": "11", "usage.completion_tokens": "7", "usage.total_tokens": "18",
				})
				if resp.Headers.Get("X-Upstream") != "kept" {
					t.Fatal("upstream headers not preserved")
				}
			}
			if len(client.requests) != 1 {
				t.Fatalf("requests=%d, want 1", len(client.requests))
			}
			wire := client.requests[0]
			if wire.URL != "https://mirror.example/provider/v1/responses" || wire.Method != http.MethodPost {
				t.Fatalf("unexpected wire request: %s %s", wire.Method, wire.URL)
			}
			if wire.Headers.Get("Authorization") != "Bearer test-key" || wire.Headers.Get("Content-Type") != "application/json" {
				t.Fatal("upstream authentication or content type changed")
			}
			wantAccept := "application/json"
			if stream {
				wantAccept = "text/event-stream"
			}
			if wire.Headers.Get("Accept") != wantAccept {
				t.Fatalf("Accept=%q", wire.Headers.Get("Accept"))
			}
			assertJSONFields(t, wire.Body, map[string]string{
				"model": "deepseek/deepseek-v4.1-flash", "input.0.role": "user", "input.0.content": "hello",
				"stream": fmt.Sprint(stream), "max_output_tokens": "64", "temperature": "0.5", "top_p": "0.8", "reasoning.effort": "high",
			})
			for _, field := range []string{"messages", "stream_options", "max_tokens", "max_completion_tokens", "reasoning_effort", "n", "stop"} {
				if gjson.GetBytes(wire.Body, field).Exists() {
					t.Errorf("chat-only field %s leaked upstream: %s", field, wire.Body)
				}
			}
		})
	}
	e := NewExecutor(&pluginConfig{}, nil)
	if e.endpoint() != upstreamBaseURL+"/responses" {
		t.Fatalf("default endpoint=%s", e.endpoint())
	}
}

func TestResponsesRequestPreservesToolHistoryAndMultimodalContent(t *testing.T) {
	body, err := chatToResponses([]byte(`{
		"model":"model","messages":[
			{"role":"system","content":"instructions"},
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA","detail":"high"}},{"type":"file","file":{"file_id":"file_1"}}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"},
			{"role":"assistant","content":[{"type":"text","text":"answer"}]}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"find","parameters":{"type":"object","properties":{"x":{"type":"integer"}}}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},"parallel_tool_calls":false,
		"response_format":{"type":"json_schema","json_schema":{"name":"result","strict":true,"schema":{"type":"object"}}},
		"reasoning":{"summary":"auto"},"reasoning_effort":"high","max_tokens":32
	}`), true)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONFields(t, body, map[string]string{
		"input.#": "5", "input.0.role": "system", "input.0.content": "instructions",
		"input.1.content.0.type": "input_text", "input.1.content.1.type": "input_image",
		"input.1.content.1.image_url": "data:image/png;base64,AAA", "input.1.content.1.detail": "high",
		"input.1.content.2.type": "input_file", "input.1.content.2.file_id": "file_1",
		"input.2.type": "function_call", "input.2.call_id": "call_1", "input.2.name": "lookup", "input.2.arguments": `{"x":1}`,
		"input.3.type": "function_call_output", "input.3.call_id": "call_1", "input.3.output": "result",
		"input.4.content.0.type": "output_text", "input.4.content.0.text": "answer",
		"tools.0.type": "function", "tools.0.name": "lookup", "tools.0.description": "find", "tools.0.strict": "false",
		"tools.0.parameters.properties.x.type": "integer", "tool_choice.type": "function", "tool_choice.name": "lookup",
		"parallel_tool_calls": "false", "text.format.type": "json_schema", "text.format.name": "result", "text.format.strict": "true",
		"text.format.schema.type": "object", "reasoning.effort": "high", "reasoning.summary": "auto", "max_output_tokens": "32",
	})
	for _, field := range []string{"tools.0.function", "tool_choice.function", "response_format", "text.format.json_schema"} {
		if gjson.GetBytes(body, field).Exists() {
			t.Errorf("nested chat field %s leaked upstream: %s", field, body)
		}
	}
}

func TestResponsesRequestExplicitStrictAndJSONMode(t *testing.T) {
	body, err := chatToResponses([]byte(`{"messages":[],"tools":[{"type":"function","function":{"name":"lookup","strict":true}}],"response_format":{"type":"json_object"},"tool_choice":"auto"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONFields(t, body, map[string]string{"tools.0.strict": "true", "text.format.type": "json_object", "tool_choice": "auto", "input": "[]", "stream": "false"})
}

func TestInvalidResponsesRequestNeverCallsUpstream(t *testing.T) {
	for _, body := range []string{
		`not-json`, `[]`, `{"messages":null}`, `{"messages":[{"role":"unknown","content":"x"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`,
		`{"messages":[],"tools":[{"type":"custom"}]}`,
	} {
		for _, stream := range []bool{false, true} {
			client := &responsesHTTPClient{}
			e := NewExecutor(parseConfig([]byte("api_key: test\n")), nil)
			req := pluginapi.ExecutorRequest{Payload: []byte(body), HTTPClient: client}
			var err error
			if stream {
				_, err = e.ExecuteStream(t.Context(), req)
			} else {
				_, err = e.Execute(t.Context(), req)
			}
			var status statusError
			if !errors.As(err, &status) || status.StatusCode() != 400 || len(client.requests) != 0 {
				t.Fatalf("body=%s stream=%v err=%v requests=%d", body, stream, err, len(client.requests))
			}
		}
	}
}

func TestResponsesToChatToolCallsAndIncompleteResults(t *testing.T) {
	body, err := responsesToChat([]byte(`{"id":"resp_tool","created_at":123,"status":"completed","output":[{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"thinking"}]},{"type":"message","content":[{"type":"output_text","text":"first"}]},{"type":"message","content":[{"type":"output_text","text":"second"},{"type":"refusal","refusal":"refused"}]},{"type":"function_call","id":"fc_internal","call_id":"call_external","name":"lookup","arguments":"{}"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`), "fallback")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONFields(t, body, map[string]string{
		"model": "fallback", "choices.0.message.content": "firstsecond", "choices.0.message.reasoning_content": "thinking",
		"choices.0.message.refusal": "refused", "choices.0.finish_reason": "tool_calls",
		"choices.0.message.tool_calls.0.id": "call_external", "choices.0.message.tool_calls.0.type": "function",
		"choices.0.message.tool_calls.0.function.name": "lookup", "choices.0.message.tool_calls.0.function.arguments": "{}",
		"usage.prompt_tokens_details.cached_tokens": "3", "usage.completion_tokens_details.reasoning_tokens": "2",
	})
	for _, test := range []struct{ native, want string }{{"max_output_tokens", "length"}, {"content_filter", "content_filter"}} {
		body, err := responsesToChat([]byte(`{"status":"incomplete","incomplete_details":{"reason":"`+test.native+`"},"output":[]}`), "model")
		if err != nil {
			t.Fatal(err)
		}
		assertJSONFields(t, body, map[string]string{"choices.0.finish_reason": test.want})
	}
}

func TestResponsesStreamTextReasoningToolsAndUsage(t *testing.T) {
	events := []string{
		`{"type":"response.created","response":{"id":"resp_test","model":"vendor/model","created_at":123,"status":"in_progress","error":null}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"think"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"think"}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"你好"}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"answer"}`,
		`{"type":"response.output_text.done","output_index":1,"content_index":0,"text":"你好answer"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_1","name":"first","arguments":""}}`,
		`{"type":"response.output_item.added","output_index":4,"item":{"type":"function_call","call_id":"call_2","name":"second","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":4,"delta":"{\"b\":"}`,
		`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"a\":1}"}`,
		`{"type":"response.function_call_arguments.delta","output_index":4,"delta":"2}"}`,
		`{"type":"response.function_call_arguments.done","output_index":2,"arguments":"{\"a\":1}"}`,
		`{"type":"response.output_item.done","output_index":4,"item":{"type":"function_call","call_id":"call_2","name":"second","arguments":"{\"b\":2}"}}`,
		`{"type":"response.completed","response":{"id":"resp_test","model":"vendor/model","created_at":123,"status":"completed","output":[{"type":"reasoning"},{"type":"message"},{"type":"function_call","call_id":"call_1","name":"first","arguments":"{\"a\":1}"},{"type":"message"},{"type":"function_call","call_id":"call_2","name":"second","arguments":"{\"b\":2}"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}}`,
	}
	raw := "event: response.created\r\n: ping\r\n"
	for i, event := range events {
		prefix := "data: "
		if i == 1 {
			prefix += "data: "
		}
		raw += prefix + event + "\r\n\r\n"
	}
	raw += "data: [DONE]\n"
	// One-byte reads exercise split prefixes, JSON, CRLF and UTF-8 characters.
	input := make([]pluginapi.HTTPStreamChunk, len(raw))
	for i := 0; i < len(raw); i++ {
		input[i] = pluginapi.HTTPStreamChunk{Payload: []byte{raw[i]}}
	}
	for _, framing := range []streamFramingPolicy{streamFramingBare, streamFramingClaude} {
		chunks := collectConvertedChunks(t, input, framing)
		var text, reasoning, finish string
		arguments, names, ids := map[int64]string{}, map[int64]string{}, map[int64]string{}
		usageCount, roleCount := 0, 0
		for _, chunk := range chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			payload := chunk.Payload
			if framing == streamFramingClaude {
				if !strings.HasPrefix(string(payload), "data: ") {
					t.Fatalf("Claude chunk lacks framing: %s", payload)
				}
				payload = payload[len("data: "):]
			}
			assertJSONFields(t, payload, map[string]string{"object": "chat.completion.chunk", "id": "resp_test", "model": "vendor/model", "created": "123"})
			root := gjson.ParseBytes(payload)
			text += root.Get("choices.0.delta.content").String()
			reasoning += root.Get("choices.0.delta.reasoning_content").String()
			if root.Get("choices.0.delta.role").String() == "assistant" {
				roleCount++
			}
			if value := root.Get("choices.0.finish_reason").String(); value != "" {
				finish = value
			}
			for _, tool := range root.Get("choices.0.delta.tool_calls").Array() {
				index := tool.Get("index").Int()
				arguments[index] += tool.Get("function.arguments").String()
				names[index] += tool.Get("function.name").String()
				ids[index] += tool.Get("id").String()
			}
			if root.Get("usage").Exists() {
				usageCount++
				assertJSONFields(t, payload, map[string]string{
					"choices": "[]", "usage.prompt_tokens": "11", "usage.completion_tokens": "7", "usage.total_tokens": "18",
					"usage.prompt_tokens_details.cached_tokens": "3", "usage.completion_tokens_details.reasoning_tokens": "2",
				})
			}
		}
		if text != "你好answer" || reasoning != "think" || finish != "tool_calls" || usageCount != 1 || roleCount != 1 {
			t.Fatalf("text=%q reasoning=%q finish=%q usage=%d role=%d", text, reasoning, finish, usageCount, roleCount)
		}
		if arguments[0] != `{"a":1}` || arguments[1] != `{"b":2}` || names[0] != "first" || names[1] != "second" || ids[0] != "call_1" || ids[1] != "call_2" {
			t.Fatalf("tool deltas duplicated or mixed: arguments=%v names=%v ids=%v", arguments, names, ids)
		}
	}
}

func TestResponsesStreamTerminalSnapshotFallback(t *testing.T) {
	input := []pluginapi.HTTPStreamChunk{{Payload: []byte(
		sseEvent(`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"ans"}`) +
			sseEvent(`{"type":"response.completed","response":`+responsesFixture+`}`),
	)}}
	var text, reasoning string
	for _, chunk := range collectConvertedChunks(t, input, streamFramingBare) {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		text += gjson.GetBytes(chunk.Payload, "choices.0.delta.content").String()
		reasoning += gjson.GetBytes(chunk.Payload, "choices.0.delta.reasoning_content").String()
	}
	if text != "answer" || reasoning != "think" {
		t.Fatalf("terminal snapshot lost or duplicated output: text=%q reasoning=%q", text, reasoning)
	}

	input = []pluginapi.HTTPStreamChunk{{Payload: []byte(sseEvent(`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`))}}
	chunks := collectConvertedChunks(t, input, streamFramingBare)
	if len(chunks) != 2 || chunks[0].Err != nil || chunks[1].Err != nil {
		t.Fatalf("terminal-only tool call: %#v", chunks)
	}
	assertJSONFields(t, chunks[0].Payload, map[string]string{"choices.0.delta.tool_calls.0.id": "call_1", "choices.0.delta.tool_calls.0.function.name": "lookup", "choices.0.delta.tool_calls.0.function.arguments": "{}"})
	assertJSONFields(t, chunks[1].Payload, map[string]string{"choices.0.finish_reason": "tool_calls"})
}

func TestResponsesStreamRejectsEmptyOrMalformedResults(t *testing.T) {
	for _, raw := range []string{
		"", ": ping\n", sseEvent(`{}`), sseEvent(`{"type":"response.completed","response":{}}`),
		sseEvent(`{"type":"response.completed","response":{"status":"in_progress","output":[]}}`),
	} {
		chunks := collectConvertedChunks(t, []pluginapi.HTTPStreamChunk{{Payload: []byte(raw)}}, streamFramingBare)
		if len(chunks) != 1 || chunks[0].Err == nil || len(chunks[0].Payload) != 0 {
			t.Fatalf("invalid stream %q was treated as success: %#v", raw, chunks)
		}
	}
}

func TestResponsesCacheWriteUsage(t *testing.T) {
	body, err := responsesToChat([]byte(`{"status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":4}}}`), "model")
	if err != nil {
		t.Fatal(err)
	}
	assertJSONFields(t, body, map[string]string{"usage.prompt_tokens_details.cached_tokens": "3", "usage.prompt_tokens_details.cached_creation_tokens": "4"})
	if gjson.GetBytes(body, "usage.prompt_tokens_details.cache_write_tokens").Exists() {
		t.Fatal("cache-write usage was not mapped to the host's canonical field")
	}
}

func TestResponsesStreamDoneFallbackAndIncompleteTail(t *testing.T) {
	input := []pluginapi.HTTPStreamChunk{{Payload: []byte(
		sseEvent(`{"type":"response.reasoning_text.done","output_index":0,"content_index":0,"text":"thinking"}`) +
			sseEvent(`{"type":"response.output_text.done","output_index":1,"content_index":0,"text":"answer"}`) +
			sseEvent(`{"type":"response.refusal.done","output_index":1,"content_index":1,"refusal":"refused"}`) +
			`data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	)}}
	chunks := collectConvertedChunks(t, input, streamFramingBare)
	if len(chunks) != 5 {
		t.Fatalf("chunks=%d, want reasoning, text, refusal, finish and usage", len(chunks))
	}
	for _, chunk := range chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
	}
	assertJSONFields(t, chunks[0].Payload, map[string]string{"choices.0.delta.reasoning_content": "thinking"})
	assertJSONFields(t, chunks[1].Payload, map[string]string{"choices.0.delta.content": "answer"})
	assertJSONFields(t, chunks[2].Payload, map[string]string{"choices.0.delta.refusal": "refused"})
	assertJSONFields(t, chunks[3].Payload, map[string]string{"choices.0.finish_reason": "length"})
	assertJSONFields(t, chunks[4].Payload, map[string]string{"usage.total_tokens": "3"})
}

func TestResponsesInBandFailureClassification(t *testing.T) {
	for _, test := range []struct {
		code   string
		status int
		retry  bool
	}{
		{"server_error", 502, true}, {"rate_limit_exceeded", 429, true}, {"insufficient_quota", 429, true},
		{"invalid_prompt", 400, false}, {"permission_denied", 403, false},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", test.code, stream), func(t *testing.T) {
				failed := `{"status":"failed","error":{"code":"` + test.code + `","message":"failed here"},"output":[]}`
				client := &responsesHTTPClient{bodies: [][]byte{[]byte(failed)}}
				client.streams = [][]pluginapi.HTTPStreamChunk{{{Payload: []byte(
					sseEvent(`{"type":"response.created","response":{"status":"in_progress"}}`) +
						sseEvent(`{"type":"response.failed","response":`+failed+`}`),
				)}}}
				e := NewExecutor(parseConfig([]byte("api_keys:\n  - key: a\n  - key: b\n")), nil)
				req := pluginapi.ExecutorRequest{Model: "deepseek-flash", Payload: []byte(`{"messages":[]}`), HTTPClient: client}
				var err error
				if stream {
					var resp pluginapi.ExecutorStreamResponse
					resp, err = e.ExecuteStream(t.Context(), req)
					if err == nil {
						for chunk := range resp.Chunks {
							if chunk.Err != nil {
								err = chunk.Err
							}
						}
					}
				} else {
					_, err = e.Execute(t.Context(), req)
				}
				if test.retry {
					if err != nil || len(client.requests) != 2 {
						t.Fatalf("retry failed: err=%v requests=%d", err, len(client.requests))
					}
					if client.requests[0].Headers.Get("Authorization") == client.requests[1].Headers.Get("Authorization") {
						t.Fatal("failure did not switch keys")
					}
				} else {
					var status statusError
					if !errors.As(err, &status) || status.StatusCode() != test.status || err.Error() != "failed here" || len(client.requests) != 1 {
						t.Fatalf("non-retryable failure changed: err=%v requests=%d", err, len(client.requests))
					}
				}
			})
		}
	}
}

func TestResponsesStreamFailureAndEOFBoundaries(t *testing.T) {
	for _, started := range []bool{false, true} {
		for _, failure := range []string{"event", "EOF"} {
			t.Run(fmt.Sprintf("started=%v/failure=%s", started, failure), func(t *testing.T) {
				raw := sseEvent(`{"type":"response.created","response":{"status":"in_progress"}}`)
				if started {
					raw += sseEvent(`{"type":"response.output_text.delta","delta":"before"}`)
				}
				if failure == "event" {
					raw += sseEvent(`{"type":"error","code":"server_error","message":"stream failed"}`)
				}
				client := &responsesHTTPClient{streams: [][]pluginapi.HTTPStreamChunk{{{Payload: []byte(raw)}}}}
				e := NewExecutor(parseConfig([]byte("api_keys:\n  - key: a\n  - key: b\n")), nil)
				resp, err := e.ExecuteStream(t.Context(), pluginapi.ExecutorRequest{
					Model: "deepseek-flash", Payload: []byte(`{"messages":[]}`), HTTPClient: client,
				})
				if err == nil {
					for chunk := range resp.Chunks {
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
				if !started {
					if err != nil || len(client.requests) != 2 {
						t.Fatalf("startup failure must retry: err=%v requests=%d", err, len(client.requests))
					}
					return
				}
				if err == nil || len(client.requests) != 1 {
					t.Fatalf("failure after output must not replay: err=%v requests=%d", err, len(client.requests))
				}
				if failure == "EOF" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("err=%v, want unexpected EOF", err)
				}
			})
		}
	}
}
