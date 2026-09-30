package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const nativeRequestFixture = `{
	"model":"deepseek-flash",
	"input":[
		{"type":"reasoning","id":"rs_previous","encrypted_content":"opaque-input","summary":[]},
		{"type":"function_call","id":"fc_previous","call_id":"call_previous","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_previous","output":"result"},
		{"role":"user","content":[{"type":"input_text","text":"你好"}]}
	],
	"previous_response_id":"resp_previous","include":["reasoning.encrypted_content"],
	"reasoning":{"effort":"high","summary":"auto"},"store":false,
	"tools":[{"type":"custom","name":"patch","format":{"type":"text"}},{"type":"web_search"}],
	"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"}}},
	"stream_options":{"include_obfuscation":false},"max_output_tokens":128,
	"metadata":{"large_integer":9007199254740993},"vendor_extension":{"enabled":true},"stream":true
}`

const nativeResponseFixture = `{"id":"resp_native","object":"response","created_at":123,"status":"completed","model":"deepseek/deepseek-v4.1-flash","output":[{"type":"reasoning","id":"rs_native","encrypted_content":"opaque-output","summary":[{"type":"summary_text","text":"think"}]},{"type":"message","id":"msg_native","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation","url":"https://example.com","title":"source","start_index":0,"end_index":6}]}]},{"type":"custom_tool_call","id":"ctc_native","call_id":"call_native","name":"patch","input":"patch data"},{"type":"web_search_call","id":"ws_native","action":{"type":"search","query":"native fields"}}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}},"vendor_extension":{"large_integer":9007199254740993}}`

func TestPluginAdvertisesBothExecutorFormats(t *testing.T) {
	description, _ := Build([]byte("api_key: test\n"))
	want := []string{executorResponsesFormat, executorChatFormat}
	if !reflect.DeepEqual(description.Capabilities.ExecutorInputFormats, want) || !reflect.DeepEqual(description.Capabilities.ExecutorOutputFormats, want) {
		t.Fatalf("formats: input=%v output=%v, want %v", description.Capabilities.ExecutorInputFormats, description.Capabilities.ExecutorOutputFormats, want)
	}
}

func TestExecutorInputAndOutputFormatsAreIndependent(t *testing.T) {
	for _, inputFormat := range []string{executorResponsesFormat, executorChatFormat} {
		for _, outputFormat := range []string{executorResponsesFormat, executorChatFormat} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s-to-%s/stream=%v", inputFormat, outputFormat, stream), func(t *testing.T) {
					client := &responsesHTTPClient{bodies: [][]byte{[]byte(nativeResponseFixture)}}
					client.streams = [][]pluginapi.HTTPStreamChunk{{{Payload: []byte(sseEvent(`{"type":"response.completed","response":` + nativeResponseFixture + `}`))}}}
					payload := []byte(nativeRequestFixture)
					if inputFormat == executorChatFormat {
						payload = []byte(`{"model":"deepseek-flash","messages":[{"role":"user","content":"hello"}]}`)
					}
					e := NewExecutor(parseConfig([]byte("api_key: test\n")), nil)
					req := pluginapi.ExecutorRequest{Model: "deepseek-flash", SourceFormat: inputFormat, Format: outputFormat, Payload: payload, HTTPClient: client}
					if stream {
						response, err := e.ExecuteStream(t.Context(), req)
						if err != nil {
							t.Fatal(err)
						}
						var chunks []pluginapi.ExecutorStreamChunk
						for chunk := range response.Chunks {
							if chunk.Err != nil {
								t.Fatal(chunk.Err)
							}
							chunks = append(chunks, chunk)
						}
						if outputFormat == executorResponsesFormat {
							events := nativeEventPayloads(t, chunks)
							if len(events) != 1 || !bytes.Equal(events[0], []byte(`{"type":"response.completed","response":`+nativeResponseFixture+`}`)) {
								t.Fatalf("native terminal response changed: %q", events)
							}
						} else {
							assertJSONFields(t, chunks[0].Payload, map[string]string{"object": "chat.completion.chunk", "choices.0.delta.reasoning_content": "think"})
						}
					} else {
						response, err := e.Execute(t.Context(), req)
						if err != nil {
							t.Fatal(err)
						}
						if outputFormat == executorResponsesFormat {
							if !bytes.Equal(response.Payload, []byte(nativeResponseFixture)) {
								t.Fatalf("native response was re-encoded: %s", response.Payload)
							}
						} else {
							assertJSONFields(t, response.Payload, map[string]string{"object": "chat.completion", "choices.0.message.content": "answer", "choices.0.message.reasoning_content": "think"})
						}
					}
					if len(client.requests) != 1 || !strings.HasSuffix(client.requests[0].URL, "/responses") {
						t.Fatalf("wire requests=%v", client.requests)
					}
					wire := client.requests[0].Body
					assertJSONFields(t, wire, map[string]string{"model": "deepseek/deepseek-v4.1-flash", "stream": fmt.Sprint(stream)})
					if inputFormat == executorResponsesFormat {
						want, err := sjson.SetBytes(payload, "model", "deepseek/deepseek-v4.1-flash")
						if err != nil {
							t.Fatal(err)
						}
						want, err = sjson.SetBytes(want, "stream", stream)
						if err != nil || !bytes.Equal(wire, want) {
							t.Fatalf("native input lost fields or precision: err=%v wire=%s want=%s", err, wire, want)
						}
					} else {
						assertJSONFields(t, wire, map[string]string{"input.0.content": "hello"})
					}
				})
			}
		}
	}
}

func TestNativeResponsesAcceptsStringInputAndBackgroundStatuses(t *testing.T) {
	for _, status := range []string{"completed", "queued", "in_progress", "cancelled", "incomplete"} {
		client := &responsesHTTPClient{bodies: [][]byte{[]byte(`{"id":"resp_background","object":"response","status":"` + status + `","output":[]}`)}}
		e := NewExecutor(parseConfig([]byte("api_key: test\n")), nil)
		response, err := e.Execute(t.Context(), pluginapi.ExecutorRequest{
			Model: "deepseek-flash", Format: executorResponsesFormat, HTTPClient: client,
			Payload: []byte(`{"model":"deepseek-flash","input":"hello","background":true}`),
		})
		if err != nil || len(client.requests) != 1 {
			t.Fatalf("status=%s err=%v requests=%d", status, err, len(client.requests))
		}
		assertJSONFields(t, response.Payload, map[string]string{"status": status, "object": "response"})
		assertJSONFields(t, client.requests[0].Body, map[string]string{"input": "hello", "background": "true", "stream": "false", "model": "deepseek/deepseek-v4.1-flash"})
	}
}

func TestNativeResponsesRejectsBadInputWithoutSending(t *testing.T) {
	for _, req := range []pluginapi.ExecutorRequest{
		{Format: executorResponsesFormat, SourceFormat: executorResponsesFormat, Payload: []byte(`not-json`)},
		{Format: executorResponsesFormat, SourceFormat: executorResponsesFormat, Payload: []byte(`[]`)},
		{Format: "unsupported", SourceFormat: executorResponsesFormat, Payload: []byte(`{"input":"hello"}`)},
		{Format: executorResponsesFormat, SourceFormat: "unsupported", Payload: []byte(`{"input":"hello"}`)},
	} {
		for _, stream := range []bool{false, true} {
			client := &responsesHTTPClient{}
			req.HTTPClient = client
			e := NewExecutor(parseConfig([]byte("api_key: test\n")), nil)
			var err error
			if stream {
				_, err = e.ExecuteStream(t.Context(), req)
			} else {
				_, err = e.Execute(t.Context(), req)
			}
			var status statusError
			if !errors.As(err, &status) || status.StatusCode() != 400 || len(client.requests) != 0 {
				t.Fatalf("request=%#v stream=%v err=%v requests=%d", req, stream, err, len(client.requests))
			}
		}
	}
}

func TestNativeResponsesStreamPreservesEveryEvent(t *testing.T) {
	events := []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"resp_native","status":"in_progress","output":[]}}`,
		`{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_native","status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_native","summary":[]}}`,
		`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"think","obfuscation":"opaque"}`,
		`{"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"think"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_native","encrypted_content":"opaque-output","summary":[{"type":"summary_text","text":"think"}]}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_native","role":"assistant","content":[]}}`,
		`{"type":"response.content_part.added","output_index":1,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}}`,
		`{"type":"response.output_text.delta","output_index":1,"content_index":0,"delta":"你好"}`,
		`{"type":"response.output_text.done","output_index":1,"content_index":0,"text":"你好"}`,
		`{"type":"response.output_item.added","output_index":2,"item":{"type":"custom_tool_call","id":"ctc_native","call_id":"call_native","name":"patch","input":""}}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":2,"delta":"patch data"}`,
		`{"type":"response.future_event","future_field":{"large_integer":9007199254740993}}`,
		`{"type":"response.completed","sequence_number":14,"response":` + nativeResponseFixture + `}`,
	}
	var raw strings.Builder
	for index, event := range events {
		raw.WriteString("event: ignored-upstream-name\r\n: ping\r\n")
		if index == 4 {
			raw.WriteString("data: ") // stacked prefix must collapse, not double-frame
		}
		raw.WriteString("data: " + event + "\r\n\r\n")
	}
	raw.WriteString("data: [DONE]\n")
	input := make([]pluginapi.HTTPStreamChunk, raw.Len())
	wire := raw.String()
	for index := 0; index < len(wire); index++ {
		input[index] = pluginapi.HTTPStreamChunk{Payload: []byte{wire[index]}}
	}
	client := &responsesHTTPClient{streams: [][]pluginapi.HTTPStreamChunk{input}}
	e := NewExecutor(parseConfig([]byte("api_key: test\n")), nil)
	response, err := e.ExecuteStream(t.Context(), pluginapi.ExecutorRequest{
		Model: "deepseek-flash", Format: executorResponsesFormat, SourceFormat: executorResponsesFormat,
		Payload: []byte(nativeRequestFixture), HTTPClient: client,
		// Framing must follow output Format, not a speculative endpoint path.
		Metadata: map[string]any{coreexecutor.RequestPathMetadataKey: "/v1/messages"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []pluginapi.ExecutorStreamChunk
	for chunk := range response.Chunks {
		chunks = append(chunks, chunk)
	}
	payloads := nativeEventPayloads(t, chunks)
	if len(payloads) != len(events) {
		t.Fatalf("events=%d, want %d", len(payloads), len(events))
	}
	for index, payload := range payloads {
		if !bytes.Equal(payload, []byte(events[index])) {
			t.Fatalf("native event %d was changed: got=%s want=%s", index, payload, events[index])
		}
		if gjson.GetBytes(payload, "choices").Exists() {
			t.Fatal("native Responses was converted to chat")
		}
	}
}

func nativeEventPayloads(t *testing.T, chunks []pluginapi.ExecutorStreamChunk) [][]byte {
	t.Helper()
	var events [][]byte
	for _, chunk := range chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		lines := bytes.Split(chunk.Payload, []byte("\n"))
		if len(lines) != 4 || !bytes.HasPrefix(lines[0], []byte("event: ")) || !bytes.HasPrefix(lines[1], []byte("data: ")) || len(lines[2]) != 0 || len(lines[3]) != 0 {
			t.Fatalf("not one complete Responses SSE event: %q", chunk.Payload)
		}
		payload := lines[1][len("data: "):]
		if !gjson.ValidBytes(payload) || gjson.GetBytes(payload, "type").String() != string(lines[0][len("event: "):]) {
			t.Fatalf("event name does not match JSON type: %q", chunk.Payload)
		}
		events = append(events, payload)
	}
	return events
}

func TestNativeResponsesFailoverKeepsPreludePrivate(t *testing.T) {
	for _, code := range []string{"server_error", "rate_limit_exceeded", "permission_denied"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", code, stream), func(t *testing.T) {
				failed := `{"status":"failed","error":{"code":"` + code + `","message":"attempt failed"},"output":[]}`
				client := &responsesHTTPClient{bodies: [][]byte{[]byte(failed)}}
				client.streams = [][]pluginapi.HTTPStreamChunk{{{Payload: []byte(
					sseEvent(`{"type":"response.created","response":{"id":"resp_abandoned","status":"in_progress"}}`) +
						sseEvent(`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_abandoned","content":[]}}`) +
						sseEvent(`{"type":"response.failed","response":`+failed+`}`),
				)}}}
				e := NewExecutor(parseConfig([]byte("api_keys:\n  - key: a\n  - key: b\n")), nil)
				req := pluginapi.ExecutorRequest{Model: "deepseek-flash", Format: executorResponsesFormat, SourceFormat: executorResponsesFormat, Payload: []byte(nativeRequestFixture), HTTPClient: client}
				var err error
				if stream {
					var response pluginapi.ExecutorStreamResponse
					response, err = e.ExecuteStream(t.Context(), req)
					if err == nil {
						var chunks []pluginapi.ExecutorStreamChunk
						for chunk := range response.Chunks {
							chunks = append(chunks, chunk)
						}
						for _, payload := range nativeEventPayloads(t, chunks) {
							if bytes.Contains(payload, []byte("abandoned")) {
								t.Fatalf("failed attempt leaked native events: %s", payload)
							}
						}
					}
				} else {
					_, err = e.Execute(t.Context(), req)
				}
				if code == "permission_denied" {
					var status statusError
					if !errors.As(err, &status) || status.StatusCode() != 403 || len(client.requests) != 1 {
						t.Fatalf("permission failure retried: err=%v requests=%d", err, len(client.requests))
					}
				} else if err != nil || len(client.requests) != 2 || client.requests[0].Headers.Get("Authorization") == client.requests[1].Headers.Get("Authorization") {
					t.Fatalf("native failover failed: err=%v requests=%d", err, len(client.requests))
				}
			})
		}
	}
}

func TestNativeResponsesStreamFailureBoundaries(t *testing.T) {
	for _, started := range []bool{false, true} {
		for _, failure := range []string{"event", "EOF", "transport", "malformed"} {
			t.Run(fmt.Sprintf("started=%v/failure=%s", started, failure), func(t *testing.T) {
				raw := sseEvent(`{"type":"response.created","response":{"id":"resp_first","status":"in_progress"}}`)
				if started {
					raw += sseEvent(`{"type":"response.output_text.delta","delta":"before"}`)
				}
				failed := `{"type":"response.failed","response":{"id":"resp_first","status":"failed","error":{"code":"server_error","message":"stream failed"},"output":[]}}`
				if failure == "event" {
					raw += sseEvent(failed)
				} else if failure == "malformed" {
					raw += "data: not-json\n"
				}
				input := []pluginapi.HTTPStreamChunk{{Payload: []byte(raw)}}
				sentinel := errors.New("upstream disconnected")
				if failure == "transport" {
					input = append(input, pluginapi.HTTPStreamChunk{Err: sentinel})
				}
				client := &responsesHTTPClient{streams: [][]pluginapi.HTTPStreamChunk{input}}
				e := NewExecutor(parseConfig([]byte("api_keys:\n  - key: a\n  - key: b\n")), nil)
				response, err := e.ExecuteStream(t.Context(), pluginapi.ExecutorRequest{Model: "deepseek-flash", Format: executorResponsesFormat, SourceFormat: executorResponsesFormat, Payload: []byte(nativeRequestFixture), HTTPClient: client})
				var chunks []pluginapi.ExecutorStreamChunk
				if err == nil {
					for chunk := range response.Chunks {
						chunks = append(chunks, chunk)
						if chunk.Err != nil {
							err = chunk.Err
						}
					}
				}
				if !started {
					if err != nil || len(client.requests) != 2 {
						t.Fatalf("native startup failure must retry: err=%v requests=%d", err, len(client.requests))
					}
					for _, payload := range nativeEventPayloads(t, chunks) {
						if bytes.Contains(payload, []byte("resp_first")) {
							t.Fatal("failed startup response ID leaked")
						}
					}
					return
				}
				if len(client.requests) != 1 || len(chunks) != 3 {
					t.Fatalf("native mid-stream failure replayed or changed: requests=%d chunks=%d err=%v", len(client.requests), len(chunks), err)
				}
				if failure == "event" {
					payloads := nativeEventPayloads(t, chunks)
					if err != nil || !bytes.Equal(payloads[2], []byte(failed)) {
						t.Fatalf("native terminal failure event was lost: err=%v event=%s", err, payloads[2])
					}
				} else if err == nil || (failure == "EOF" && !errors.Is(err, io.ErrUnexpectedEOF)) || (failure == "transport" && !errors.Is(err, sentinel)) {
					t.Fatalf("native stream error not propagated: %v", err)
				}
			})
		}
	}
}

func TestNativeResponsesConverterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	input := make(chan pluginapi.HTTPStreamChunk)
	output := convertChunks(ctx, input, streamFramingBare, "model", executorResponsesFormat)
	cancel()
	select {
	case chunk := <-output:
		if !errors.Is(chunk.Err, context.Canceled) || len(chunk.Payload) != 0 {
			t.Fatalf("cancellation=%#v", chunk)
		}
	case <-time.After(time.Second):
		t.Fatal("native converter did not stop on cancellation")
	}
	select {
	case _, open := <-output:
		if open {
			t.Fatal("native converter emitted more data after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("native converter did not close")
	}
	close(input)
}

func TestCountTokensMatchesOutputFormat(t *testing.T) {
	e := NewExecutor(&pluginConfig{}, nil)
	for _, format := range []string{executorResponsesFormat, executorChatFormat} {
		response, err := e.CountTokens(t.Context(), pluginapi.ExecutorRequest{Format: format, Model: "model", Payload: []byte(`{"input":"hello"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if format == executorResponsesFormat {
			assertJSONFields(t, response.Payload, map[string]string{"object": "response", "status": "completed", "output": "[]", "usage.input_tokens": "4", "usage.output_tokens": "0", "usage.total_tokens": "4"})
		} else {
			assertJSONFields(t, response.Payload, map[string]string{"object": "chat.completion", "choices": "[]", "usage.prompt_tokens": "4", "usage.completion_tokens": "0", "usage.total_tokens": "4"})
		}
	}
}

func TestTranslatorPreservesNativeResponses(t *testing.T) {
	translator := NewTranslator(parseConfig(nil))
	request, err := translator.TranslateRequest(t.Context(), pluginapi.RequestTransformRequest{FromFormat: executorResponsesFormat, ToFormat: "commandcode", Model: "deepseek-flash", Body: []byte(nativeRequestFixture)})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONFields(t, request.Body, map[string]string{"model": "deepseek/deepseek-v4.1-flash", "input.0.encrypted_content": "opaque-input", "tools.0.type": "custom", "metadata.large_integer": "9007199254740993"})
	response, err := translator.TranslateResponse(t.Context(), pluginapi.ResponseTransformRequest{FromFormat: executorResponsesFormat, ToFormat: executorResponsesFormat, Body: []byte(nativeResponseFixture)})
	if err != nil || !bytes.Equal(response.Body, []byte(nativeResponseFixture)) {
		t.Fatalf("native response translator changed payload: err=%v body=%s", err, response.Body)
	}
	if _, err := translator.TranslateResponse(t.Context(), pluginapi.ResponseTransformRequest{FromFormat: executorResponsesFormat, ToFormat: executorChatFormat, Body: []byte(nativeResponseFixture)}); err == nil {
		t.Fatal("translator silently passed native Responses off as chat")
	}
}
