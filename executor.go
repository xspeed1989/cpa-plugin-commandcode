package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Executor always calls upstream Responses. Host Responses input/output stay
// native; Chat Completions input/output are converted independently as needed.
//
// v0.2.0: requests go through the weighted multi-key pool. Members without
// proxy_url use the host HTTP client (host proxy policy + request-log
// preserved); members with proxy_url use a self-built transport (host
// request-log cannot capture those). Retryable failures (transport errors,
// 401/402/429/5xx, and quota-exhausted bodies on other statuses) try the next
// pool member, for at most three rounds with cancellable backoff.
type Executor struct {
	cfg        *pluginConfig
	translator *Translator
	keypool    *pool
}

func NewExecutor(cfg *pluginConfig, t *Translator) *Executor {
	if t == nil {
		t = NewTranslator(cfg)
	}
	return &Executor{cfg: cfg, translator: t, keypool: newPool()}
}

func (e *Executor) Identifier() string { return Provider }

// apiKey keeps the v0.1.x single-key resolution for translator paths and
// error messages. Live execution uses the pool (members method).
func apiKey(cfg *pluginConfig, req pluginapi.ExecutorRequest) string {
	if ms := cfg.members(req); len(ms) > 0 {
		return strings.TrimSpace(ms[0].Key)
	}
	return ""
}

const missingKeyMsg = "commandcode executor: missing api key (router path passes nil auth; set plugins.configs.commandcode.api_key or api_keys in config.yaml)"

const claudeMessagesPath = "/v1/messages"

type upstreamStreamDecoder interface {
	convert([]byte) ([][]byte, error)
	terminal() bool
	eofError() error
}

type streamFramingPolicy uint8

const (
	streamFramingBare streamFramingPolicy = iota
	streamFramingClaude
)

func streamFramingForRequest(req pluginapi.ExecutorRequest) streamFramingPolicy {
	path, ok := req.Metadata[coreexecutor.RequestPathMetadataKey].(string)
	if ok && path == claudeMessagesPath {
		return streamFramingClaude
	}
	return streamFramingBare
}

func (p streamFramingPolicy) apply(payload []byte) []byte {
	if p != streamFramingClaude {
		return payload
	}
	framed := make([]byte, 0, len("data: ")+len(payload))
	framed = append(framed, "data: "...)
	framed = append(framed, payload...)
	return framed
}

func (e *Executor) endpoint() string {
	return strings.TrimRight(e.cfg.baseURL(), "/") + "/responses"
}

// SourceFormat selects input handling, independently of the output Format.
// Native Responses keeps all input fields; only chat input needs conversion.
func (e *Executor) buildUpstreamBody(ctx context.Context, req pluginapi.ExecutorRequest, stream bool) ([]byte, error) {
	inputFormat := inputFormatForRequest(req)
	if inputFormat != executorChatFormat && inputFormat != executorResponsesFormat {
		return nil, statusError{statusCode: http.StatusBadRequest, msg: fmt.Sprintf("commandcode executor: unsupported input format %q", inputFormat)}
	}
	outputFormat := strings.ToLower(strings.TrimSpace(req.Format))
	if outputFormat != "" && outputFormat != executorChatFormat && outputFormat != executorResponsesFormat {
		return nil, statusError{statusCode: http.StatusBadRequest, msg: fmt.Sprintf("commandcode executor: unsupported output format %q", outputFormat)}
	}
	out, err := e.translator.TranslateRequest(ctx, pluginapi.RequestTransformRequest{
		FromFormat: inputFormat,
		ToFormat:   "commandcode",
		Model:      req.Model,
		Stream:     stream,
		Body:       req.Payload,
	})
	if err != nil {
		return nil, err
	}
	if inputFormat == executorResponsesFormat {
		return nativeResponsesRequest(out.Body, stream)
	}
	return chatToResponses(out.Body, stream)
}

func upstreamHeaders(apiKey string, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("User-Agent", "cli-proxy-commandcode")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	return h
}

const maxFailoverRounds = 3

// waitForRetryRound backs off for 1s before round two and 2s before round
// three. Cancellation always wins over starting another upstream attempt.
func waitForRetryRound(ctx context.Context, round int) error {
	if round == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(time.Duration(round) * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Execute tries every available member once per round on retryable errors.
func (e *Executor) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	members := e.cfg.members(req)
	if len(members) == 0 {
		return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}
	body, err := e.buildUpstreamBody(ctx, req, false)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	var lastErr error
	for round := 0; round < maxFailoverRounds; round++ {
		if err := waitForRetryRound(ctx, round); err != nil {
			return pluginapi.ExecutorResponse{}, err
		}
		for _, idx := range e.keypool.order(members) {
			if err := ctx.Err(); err != nil {
				return pluginapi.ExecutorResponse{}, err
			}
			m := members[idx]
			d, err := e.keypool.clientFor(idx, m, req.HTTPClient)
			if err != nil {
				lastErr = err
				continue
			}
			status, headers, respBody, err := d.do(ctx, e.endpoint(), upstreamHeaders(strings.TrimSpace(m.Key), false), body)
			if ctx.Err() != nil {
				return pluginapi.ExecutorResponse{}, ctx.Err()
			}
			if err != nil {
				lastErr = err
				continue
			}
			if status < 200 || status >= 300 {
				lastErr = statusError{statusCode: status, body: respBody}
				if !retryable(status, nil, respBody) {
					return pluginapi.ExecutorResponse{}, lastErr
				}
				continue
			}
			var fixed []byte
			if wantsNativeResponses(req) {
				fixed, err = nativeResponsesBody(respBody)
			} else {
				fixed, err = responsesToChat(respBody, req.Model)
			}
			if err != nil {
				lastErr = err
				if !retryableExecutionError(err) {
					return pluginapi.ExecutorResponse{}, err
				}
				continue
			}
			return pluginapi.ExecutorResponse{Payload: fixed, Headers: headers}, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	if lastErr != nil {
		return pluginapi.ExecutorResponse{}, lastErr
	}
	return pluginapi.ExecutorResponse{}, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: all pool members failed"}
}

// ExecuteStream emits native Responses SSE or converted chat chunks according
// to Format. Chat /v1/messages receives the host translator's data: prefix.
// Failover applies only before useful output; native lifecycle events are
// buffered until then so failed attempts cannot leak response IDs. Once output
// starts, a failure never replays delivered text, events or tool calls.
func (e *Executor) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	framing := streamFramingForRequest(req)
	members := e.cfg.members(req)
	if len(members) == 0 {
		return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}
	body, err := e.buildUpstreamBody(ctx, req, true)
	if err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	var lastErr error
	for round := 0; round < maxFailoverRounds; round++ {
		if err := waitForRetryRound(ctx, round); err != nil {
			return pluginapi.ExecutorStreamResponse{}, err
		}
		for _, idx := range e.keypool.order(members) {
			if err := ctx.Err(); err != nil {
				return pluginapi.ExecutorStreamResponse{}, err
			}
			m := members[idx]
			d, err := e.keypool.clientFor(idx, m, req.HTTPClient)
			if err != nil {
				lastErr = err
				continue
			}
			attemptCtx, cancel := context.WithCancel(ctx)
			status, headers, chunks, err := d.doStream(attemptCtx, e.endpoint(), upstreamHeaders(strings.TrimSpace(m.Key), true), body)
			if err != nil {
				cancel()
				lastErr = err
				continue
			}
			if status < 200 || status >= 300 {
				errBody := readStreamErrorBody(attemptCtx, chunks)
				cancel()
				lastErr = statusError{statusCode: status, body: errBody}
				if !retryable(status, nil, errBody) {
					return pluginapi.ExecutorStreamResponse{}, lastErr
				}
				continue
			}
			output, err := primeStream(attemptCtx, cancel, convertChunks(attemptCtx, chunks, framing, req.Model, req.Format))
			if err != nil {
				lastErr = err
				if !retryableExecutionError(err) {
					return pluginapi.ExecutorStreamResponse{}, err
				}
				continue
			}
			return pluginapi.ExecutorStreamResponse{Headers: headers, Chunks: output}, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return pluginapi.ExecutorStreamResponse{}, err
	}
	if lastErr != nil {
		return pluginapi.ExecutorStreamResponse{}, lastErr
	}
	return pluginapi.ExecutorStreamResponse{}, statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: all pool members failed"}
}

// In-band Responses failures follow the same classification as HTTP errors.
// Transport/read errors retain the existing retry policy.
func retryableExecutionError(err error) bool {
	var failure statusError
	if errors.As(err, &failure) {
		return retryable(failure.statusCode, nil, failure.body)
	}
	return true
}

// primeStream waits until output is available so an initial stream error can
// still trigger failover. It owns cancel, including the successful stream's
// lifetime, and forwards the first chunk exactly once.
func primeStream(ctx context.Context, cancel context.CancelFunc, in <-chan pluginapi.ExecutorStreamChunk) (<-chan pluginapi.ExecutorStreamChunk, error) {
	var first pluginapi.ExecutorStreamChunk
	var ok bool
	select {
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	case first, ok = <-in:
	}
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}
	if first.Err != nil {
		cancel()
		return nil, first.Err
	}
	if !ok {
		cancel()
		return in, nil
	}
	out := make(chan pluginapi.ExecutorStreamChunk, 1)
	out <- first
	go func() {
		defer close(out)
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case chunk, ok := <-in:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					return
				case out <- chunk:
				}
			}
		}
	}()
	return out, nil
}

// convertChunks shares SSE buffering, cancellation and size limits between
// native Responses and converted chat output. Native frames already carry
// event:/data: framing; only chat frames use the route-specific framing policy.
// Upstream [DONE] is swallowed; native Responses carries its own terminal event.
func convertChunks(ctx context.Context, in <-chan pluginapi.HTTPStreamChunk, framing streamFramingPolicy, model, outputFormat string) <-chan pluginapi.ExecutorStreamChunk {
	// One slot lets a terminal cancellation error be reported even when the
	// caller stops draining the stream at the same time.
	out := make(chan pluginapi.ExecutorStreamChunk, 1)
	go func() {
		defer close(out)
		var pending []byte
		native := strings.ToLower(strings.TrimSpace(outputFormat)) == executorResponsesFormat
		var decoder upstreamStreamDecoder = &responseStream{model: model}
		if native {
			decoder = &nativeResponseStream{}
			framing = streamFramingBare
		}
		emitError := func(err error) {
			terminal := pluginapi.ExecutorStreamChunk{Err: err}
			select {
			case out <- terminal:
			case <-ctx.Done():
				select {
				case out <- terminal:
				default:
				}
			}
		}
		processLine := func(line []byte) bool {
			payload := streamLinePayload(line)
			if len(payload) > 0 && !json.Valid(payload) {
				if native {
					emitError(statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: invalid Responses SSE JSON"})
					return false
				}
				return true
			}
			frames, err := decoder.convert(payload)
			if err != nil {
				emitError(err)
				return false
			}
			for _, frame := range frames {
				select {
				case <-ctx.Done():
					return false
				case out <- pluginapi.ExecutorStreamChunk{Payload: framing.apply(frame)}:
				}
			}
			return !decoder.terminal()
		}
		for {
			select {
			case <-ctx.Done():
				emitError(ctx.Err())
				return
			case chunk, ok := <-in:
				if !ok {
					if processLine(pending) {
						if err := decoder.eofError(); err != nil {
							emitError(err)
						}
					}
					return
				}
				if chunk.Err != nil {
					emitError(chunk.Err)
					return
				}
				pending = append(pending, chunk.Payload...)
				for {
					idx := bytes.IndexByte(pending, '\n')
					if idx < 0 {
						break
					}
					line := pending[:idx+1]
					pending = pending[idx+1:]
					if !processLine(line) {
						return
					}
				}
				// Never silently discard an oversized, unfinished event.
				if len(pending) > 4<<20 {
					emitError(statusError{statusCode: http.StatusBadGateway, msg: "commandcode executor: SSE line exceeds 4 MiB"})
					return
				}
			}
		}
	}()
	return out
}

// streamLinePayload strips SSE transport prefixes without touching JSON.
// Control lines, empty data and [DONE] yield nil. The caller validates JSON.
func streamLinePayload(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		// Non-SSE bytes (e.g. event: lines, comments): drop, never corrupt.
		return nil
	}
	payload := trimmed
	for bytes.HasPrefix(bytes.TrimSpace(payload), []byte("data:")) {
		p := bytes.TrimSpace(payload)
		payload = bytes.TrimSpace(p[len("data:"):])
	}
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return nil
	}
	return payload
}

// normalizeStreamLine keeps the legacy chat normalization contract for helpers.
func normalizeStreamLine(line []byte) []byte {
	payload := streamLinePayload(line)
	if !json.Valid(payload) {
		return nil
	}
	fixed, _ := mapReasoningBody(payload)
	return fixed
}

// normalizeStreamBytes handles one raw host-stream read (kept for unit
// tests and non-streaming helpers): same bare-JSON contract as
// normalizeStreamLine, joined back with newlines for assertion convenience.
func normalizeStreamBytes(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw
	}
	lines := bytes.Split(raw, []byte("\n"))
	var out [][]byte
	for _, line := range lines {
		if frame := normalizeStreamLine(line); len(bytes.TrimSpace(frame)) > 0 {
			out = append(out, frame)
		}
	}
	if len(out) == 0 {
		return raw
	}
	return bytes.Join(out, []byte("\n"))
}

// CountTokens is a local estimate; commandcode exposes no tokenize endpoint.
func (e *Executor) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	_ = ctx
	count := int64(len(req.Payload) / 4)
	if count < 1 && len(req.Payload) > 0 {
		count = 1
	}
	if wantsNativeResponses(req) {
		raw, _ := json.Marshal(map[string]any{
			"id": "commandcode-count", "object": "response", "created_at": 0,
			"model": req.Model, "status": "completed", "output": []any{},
			"usage": map[string]any{"input_tokens": count, "output_tokens": 0, "total_tokens": count},
		})
		return pluginapi.ExecutorResponse{Payload: raw}, nil
	}
	usage := map[string]any{
		"prompt_tokens":     count,
		"completion_tokens": 0,
		"total_tokens":      count,
	}
	raw, _ := json.Marshal(map[string]any{
		"id":      "commandcode-count",
		"object":  "chat.completion",
		"created": 0,
		"model":   req.Model,
		"choices": []any{},
		"usage":   usage,
	})
	return pluginapi.ExecutorResponse{Payload: raw}, nil
}

// HttpRequest bridges raw executor HTTP through the pool: first member's
// transport (pool order is stable per call) with the resolved api key
// injected when the caller did not set Authorization.
func (e *Executor) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	if strings.TrimSpace(req.URL) == "" {
		return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("commandcode executor: request URL is required")
	}
	headers := req.Headers.Clone()
	if headers == nil {
		headers = http.Header{}
	}
	members := e.cfg.members(poolReqFromHTTP(req))
	if len(members) == 0 && headers.Get("Authorization") == "" {
		return pluginapi.ExecutorHTTPResponse{}, statusError{statusCode: http.StatusUnauthorized, msg: missingKeyMsg}
	}
	if headers.Get("Authorization") == "" {
		headers.Set("Authorization", "Bearer "+strings.TrimSpace(members[0].Key))
	}
	var d doer
	if len(members) > 0 {
		var err error
		d, err = e.keypool.clientFor(0, members[0], req.HTTPClient)
		if err != nil {
			return pluginapi.ExecutorHTTPResponse{}, err
		}
	} else {
		if req.HTTPClient == nil {
			return pluginapi.ExecutorHTTPResponse{}, fmt.Errorf("commandcode executor: host HTTP client is required")
		}
		d = hostDoer{client: req.HTTPClient}
	}
	status, respHeaders, respBody, err := d.do(ctx, strings.TrimSpace(req.URL), headers, req.Body)
	if err != nil {
		return pluginapi.ExecutorHTTPResponse{}, err
	}
	return pluginapi.ExecutorHTTPResponse{StatusCode: status, Headers: respHeaders, Body: respBody}, nil
}

// statusError carries an upstream HTTP status back to the host (the ABI
// error envelope preserves it as http_status for retry classification).
type statusError struct {
	statusCode int
	msg        string
	body       []byte
}

func (e statusError) Error() string {
	if strings.TrimSpace(e.msg) != "" {
		return e.msg
	}
	if len(e.body) > 0 {
		return upstreamErrorMessage(e.body)
	}
	return fmt.Sprintf("status %d", e.statusCode)
}

func (e statusError) StatusCode() int { return e.statusCode }

func upstreamErrorMessage(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var decoded struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
		if len(decoded.Error) > 0 {
			var obj struct {
				Message string `json:"message"`
			}
			if errObj := json.Unmarshal(decoded.Error, &obj); errObj == nil && strings.TrimSpace(obj.Message) != "" {
				return strings.TrimSpace(obj.Message)
			}
			var s string
			if errStr := json.Unmarshal(decoded.Error, &s); errStr == nil && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		if strings.TrimSpace(decoded.Message) != "" {
			return strings.TrimSpace(decoded.Message)
		}
	}
	if len(trimmed) > 500 {
		return trimmed[:500]
	}
	return trimmed
}

func readStreamErrorBody(ctx context.Context, chunks <-chan pluginapi.HTTPStreamChunk) []byte {
	const maxBytes = 1 << 20
	body := make([]byte, 0)
	if chunks == nil {
		return body
	}
	for len(body) < maxBytes {
		select {
		case <-ctx.Done():
			return body
		case chunk, ok := <-chunks:
			if !ok {
				return body
			}
			if len(chunk.Payload) > 0 {
				remaining := maxBytes - len(body)
				if len(chunk.Payload) > remaining {
					return append(body, chunk.Payload[:remaining]...)
				}
				body = append(body, chunk.Payload...)
			}
			if chunk.Err != nil {
				return body
			}
		}
	}
	return body
}
