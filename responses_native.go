package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func wantsNativeResponses(req pluginapi.ExecutorRequest) bool {
	return strings.ToLower(strings.TrimSpace(req.Format)) == executorResponsesFormat
}

// Format is the selected OUTPUT protocol, not the input protocol. The host
// supplies SourceFormat independently; only direct callers may omit it.
func inputFormatForRequest(req pluginapi.ExecutorRequest) string {
	if source := strings.ToLower(strings.TrimSpace(req.SourceFormat)); source != "" {
		return source
	}
	if wantsNativeResponses(req) && !gjson.GetBytes(req.Payload, "messages").Exists() {
		return executorResponsesFormat
	}
	return executorChatFormat
}

func nativeResponsesRequest(body []byte, stream bool) ([]byte, error) {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, statusError{statusCode: http.StatusBadRequest, msg: "commandcode executor: expected a Responses JSON object"}
	}
	// Do not decode/re-encode native input: opaque reasoning, custom tools,
	// extension fields and large JSON integers must survive unchanged.
	return sjson.SetBytes(body, "stream", stream)
}

func nativeResponsesBody(body []byte) ([]byte, error) {
	root := gjson.ParseBytes(body)
	if !gjson.ValidBytes(body) || !root.IsObject() {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: invalid Responses JSON"}
	}
	if err := responseFailure(root); err != nil {
		return nil, err
	}
	if !root.Get("output").IsArray() {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: Responses output is missing"}
	}
	switch root.Get("status").String() {
	case "completed", "incomplete", "queued", "in_progress", "cancelled":
		return bytes.Clone(body), nil
	default:
		return nil, statusError{statusCode: http.StatusBadGateway, msg: fmt.Sprintf("commandcode executor: unexpected Responses status %q", root.Get("status").String())}
	}
}

type nativeResponseStream struct {
	started      bool
	finished     bool
	prelude      [][]byte
	preludeBytes int
}

func (s *nativeResponseStream) terminal() bool { return s.finished }

func (s *nativeResponseStream) eofError() error {
	if s.finished {
		return nil
	}
	return io.ErrUnexpectedEOF
}

func (s *nativeResponseStream) convert(payload []byte) ([][]byte, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	event := gjson.ParseBytes(payload)
	kind := event.Get("type").String()
	if !event.IsObject() || (!strings.HasPrefix(kind, "response.") && kind != "error") || strings.ContainsAny(kind, "\r\n") {
		return nil, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: invalid Responses event type"}
	}
	frame := responsesSSEFrame(kind, payload)
	if err := responseEventFailure(event); err != nil {
		if !s.started {
			return nil, err
		}
		// Preserve the original terminal failure event after output starts.
		// The host's Responses SSE framer recognizes it as a terminal error;
		// replaying or synthesizing a second event would corrupt the stream.
		s.finished = true
		return [][]byte{frame}, nil
	}
	switch kind {
	case "response.completed", "response.incomplete":
		if err := validateResponseResult(event.Get("response")); err != nil {
			return nil, err
		}
		s.finished = true
	}
	if !s.started && !s.finished && isResponsePrelude(event) {
		// Keep lifecycle events private until useful output (or a successful
		// terminal event) is available. A failed attempt must not leak its ID,
		// sequence numbers or output-item headers before key failover.
		s.preludeBytes += len(frame)
		if s.preludeBytes > 4<<20 {
			return nil, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: Responses prelude exceeds 4 MiB"}
		}
		s.prelude = append(s.prelude, frame)
		return nil, nil
	}
	s.started = true
	frames := append(s.prelude, frame)
	s.prelude = nil
	s.preludeBytes = 0
	return frames, nil
}

// Both output modes classify in-band failures identically before output.
func responseEventFailure(event gjson.Result) error {
	switch event.Get("type").String() {
	case "error":
		if event.Get("error").Type != gjson.Null {
			return responseFailure(event)
		}
		body, _ := json.Marshal(map[string]any{"error": event.Value()})
		return responseFailure(gjson.ParseBytes(body))
	case "response.failed":
		if err := responseFailure(event.Get("response")); err != nil {
			return err
		}
		return statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: upstream response failed"}
	default:
		return responseFailure(event.Get("response"))
	}
}

func isResponsePrelude(event gjson.Result) bool {
	switch event.Get("type").String() {
	case "response.created", "response.in_progress":
		return true
	case "response.output_item.added":
		kind := event.Get("item.type").String()
		return (kind == "message" || kind == "reasoning") && event.Get("item.content.#").Int() == 0 && event.Get("item.summary.#").Int() == 0
	case "response.content_part.added", "response.reasoning_summary_part.added":
		return event.Get("part.text").String() == ""
	case "response.output_text.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.refusal.delta":
		return event.Get("delta").String() == ""
	}
	return false
}

// Native Responses must be complete SSE events. The host forwards them to
// the Responses handler without the Chat Completions transport framing path.
func responsesSSEFrame(kind string, payload []byte) []byte {
	out := make([]byte, 0, len(kind)+len(payload)+len("event: \ndata: \n\n"))
	out = append(out, "event: "...)
	out = append(out, kind...)
	out = append(out, "\ndata: "...)
	out = append(out, payload...)
	return append(out, '\n', '\n')
}
