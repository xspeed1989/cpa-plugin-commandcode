package plugin

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Router handles configured models only in the commandcode/ namespace.
// The host consults routers in priority order before built-in provider
// resolution; returning Handled=false falls through to the normal
// openai-compatibility path (safe default during rollout).
type Router struct {
	cfg *pluginConfig
}

func NewRouter(cfg *pluginConfig) *Router { return &Router{cfg: cfg} }

// routingModelKey strips whitespace and thinking suffixes, but preserves the
// provider namespace so another provider's model cannot match by basename.
func routingModelKey(model string) string {
	m := strings.TrimSpace(model)
	if i := strings.Index(m, "("); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	return strings.ToLower(m)
}

func (r *Router) owned(req pluginapi.ModelRouteRequest) bool {
	r.cfg.ensureIndexes()
	model := req.RequestedModel
	if strings.TrimSpace(model) == "" {
		model = modelFromBody(req.Body)
	}
	_, owned := r.cfg.modelSet()[routingModelKey(model)]
	return owned
}

// RouteModel routes commandcode models to this plugin's own executor.
func (r *Router) RouteModel(ctx context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	_ = ctx
	if !r.owned(req) {
		return pluginapi.ModelRouteResponse{}, nil
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "commandcode provider plugin",
	}, nil
}

// modelFromBody extracts the payload model only when RequestedModel is absent.
// An explicit RequestedModel is authoritative; the body cannot widen ownership.
func modelFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// Minimal scan without failing the route on malformed JSON.
	marker := []byte(`"model"`)
	idx := -1
	for i := 0; i+len(marker) <= len(body); i++ {
		match := true
		for j := range marker {
			if body[i+j] != marker[j] {
				match = false
				break
			}
		}
		if match {
			idx = i + len(marker)
			break
		}
	}
	if idx < 0 {
		return ""
	}
	rest := body[idx:]
	// skip whitespace, colon, whitespace, quote
	p := 0
	for p < len(rest) && (rest[p] == ' ' || rest[p] == '\t' || rest[p] == '\r' || rest[p] == '\n' || rest[p] == ':') {
		p++
	}
	if p >= len(rest) || rest[p] != '"' {
		return ""
	}
	p++
	var b strings.Builder
	for ; p < len(rest); p++ {
		c := rest[p]
		if c == '\\' && p+1 < len(rest) {
			p++
			b.WriteByte(rest[p])
			continue
		}
		if c == '"' {
			return b.String()
		}
		b.WriteByte(c)
	}
	return ""
}
