package plugin

import (
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Translator converts between host canonical formats and the commandcode
// canonical envelopes. Model normalization applies to both Chat Completions
// and Responses; legacy reasoning backfill only applies to chat payloads.
// The executor owns upstream wire conversion and output-format selection.
type Translator struct {
	cfg *pluginConfig
}

func NewTranslator(cfg *pluginConfig) *Translator { return &Translator{cfg: cfg} }

// TranslateRequest handles canonical -> commandcode. Supported edges:
// openai->commandcode, claude->commandcode, openai-response->commandcode.
// The executor supplies its selected input format. Native Responses input
// receives the same model-alias rewrite without changing its schema.
func (t *Translator) TranslateRequest(ctx context.Context, req pluginapi.RequestTransformRequest) (pluginapi.PayloadResponse, error) {
	_ = ctx
	from := strings.ToLower(strings.TrimSpace(req.FromFormat))
	to := strings.ToLower(strings.TrimSpace(req.ToFormat))
	if to != "" && to != "commandcode" && to != executorChatFormat && to != executorResponsesFormat {
		return pluginapi.PayloadResponse{}, fmt.Errorf("unsupported request translation %s -> %s", req.FromFormat, req.ToFormat)
	}
	switch from {
	case "", executorChatFormat, executorResponsesFormat, "commandcode":
		return pluginapi.PayloadResponse{Body: t.normalizeRequestModel(req.Model, req.Body)}, nil
	default:
		return pluginapi.PayloadResponse{Body: append([]byte(nil), req.Body...)}, nil
	}
}

// TranslateResponse handles commandcode -> canonical. Supported edges:
// commandcode->openai, commandcode->claude, commandcode->openai-response.
// The reasoning backfill runs for every edge (it is format-agnostic JSON);
// the host's own translators then render thinking blocks downstream.
func (t *Translator) TranslateResponse(ctx context.Context, req pluginapi.ResponseTransformRequest) (pluginapi.PayloadResponse, error) {
	_ = ctx
	from := strings.ToLower(strings.TrimSpace(req.FromFormat))
	to := strings.ToLower(strings.TrimSpace(req.ToFormat))
	if from == executorResponsesFormat {
		if to == "" || to == executorResponsesFormat || to == "commandcode" {
			return pluginapi.PayloadResponse{Body: append([]byte(nil), req.Body...)}, nil
		}
		return pluginapi.PayloadResponse{}, fmt.Errorf("unsupported response translation %s -> %s", req.FromFormat, req.ToFormat)
	}
	if from != "" && from != "commandcode" && from != executorChatFormat {
		return pluginapi.PayloadResponse{}, fmt.Errorf("unsupported response translation %s -> %s", req.FromFormat, req.ToFormat)
	}
	if to != "" && to != "openai" && to != "claude" && to != "openai-response" && to != "commandcode" {
		return pluginapi.PayloadResponse{}, fmt.Errorf("unsupported response translation %s -> %s", req.FromFormat, req.ToFormat)
	}
	fixed, _ := mapReasoningBody(req.Body)
	return pluginapi.PayloadResponse{Body: fixed}, nil
}

// normalizeRequestModel rewrites the outbound model to the vendor's name.
//
// This plugin runs its own executor against its own base URL, so the host's
// openai-compatibility alias table never applies to the requests it sends: a
// bare alias reaches commandcode unchanged and is rejected with
// `Model "deepseek-flash" is not supported on this endpoint`. The alias ->
// vendor-name mapping therefore has to happen here, driven by the configured
// `models:` list.
//
// The comparison is literal, not normalized. A vendor name like
// "z-ai/glm-5.3-flash" and the alias "glm-5.3-flash" normalize to the same
// string, but the prefix is significant upstream -- treating them as equal
// would skip the rewrite and send a name the vendor does not serve.
func (t *Translator) normalizeRequestModel(model string, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	t.cfg.ensureIndexes()
	vendorName := t.cfg.upstreamName(model)
	if vendorName == "" {
		return body
	}
	if current := strings.TrimSpace(gjsonGetString(body, "model")); current == vendorName {
		return body
	}
	if out, err := sjsonSetString(body, "model", vendorName); err == nil {
		return out
	}
	return body
}
