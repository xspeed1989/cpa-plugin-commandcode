package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRouterRespectsModelNamespaces(t *testing.T) {
	cases := []struct {
		model string
		owned bool
	}{
		{"deepseek-flash", false},
		{"deepseek-flash(high)", false},
		{"deepseek-v4.1-flash", false},
		{"deepseek/deepseek-v4.1-flash", false},
		{"commandcode/deepseek-flash", true},
		{"commandcode/deepseek-v4.1-flash", true},
		{"commandcode/deepseek/deepseek-v4.1-flash(high)", true},
		{"glm-5.3-flash", false},
		{"z-ai/glm-5.3-flash", false},
		{"commandcode/z-ai/glm-5.3-flash", true},
		{" COMMANDCODE/DEEPSEEK-FLASH(high) ", true},
		{"opencode-go/deepseek-v4.1-flash", false},
		{"opencode-go/deepseek-v4.1-flash(high)", false},
		{"opencode-go/deepseek-flash", false},
		{"opencode-go/glm-5.3-flash", false},
		{"other/deepseek/deepseek-v4.1-flash", false},
		{"other/z-ai/glm-5.3-flash", false},
		{"other/deepseek-flash", false},
		{"commandcode/opencode-go/deepseek-v4.1-flash", false},
		{"commandcode/gpt-5", false},
		{"commandcode-extra/deepseek-flash", false},
		{"prefix/commandcode/deepseek-flash", false},
		{"commandcode", false},
		{"commandcode/", false},
		{"gpt-5", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"model": tc.model})
			if err != nil {
				t.Fatal(err)
			}
			for _, source := range []string{"requested", "body", "both"} {
				t.Run(source, func(t *testing.T) {
					req := pluginapi.ModelRouteRequest{}
					if source != "body" {
						req.RequestedModel = tc.model
					}
					if source != "requested" {
						req.Body = body
					}
					router := NewRouter(&pluginConfig{})
					resp, err := router.RouteModel(t.Context(), req)
					if err != nil {
						t.Fatal(err)
					}
					if resp.Handled != tc.owned {
						t.Fatalf("model %q: Handled=%v, want %v", tc.model, resp.Handled, tc.owned)
					}
					if resp.Handled && resp.TargetKind != pluginapi.ModelRouteTargetSelf {
						t.Fatalf("TargetKind=%q, want self", resp.TargetKind)
					}
				})
			}
		})
	}
}

func TestRouterRespectsConfiguredModelNamespaces(t *testing.T) {
	cfg := parseConfig([]byte("models:\n  - alias: team/fast\n    name: vendor/model-name\n  - alias: team/passthrough\n"))
	router := NewRouter(cfg)
	for _, tc := range []struct {
		model string
		owned bool
	}{
		{"team/fast", false},
		{"vendor/model-name", false},
		{"commandcode/team/fast(high)", true},
		{"commandcode/vendor/model-name", true},
		{"commandcode/fast", true},
		{"commandcode/model-name", true},
		{"fast", false},
		{"model-name", false},
		{"team/passthrough", false},
		{"commandcode/team/passthrough", true},
		{"other/fast", false},
		{"other/model-name", false},
		{"other/passthrough", false},
		{"opencode-go/deepseek-v4.1-flash", false},
	} {
		t.Run(tc.model, func(t *testing.T) {
			resp, err := router.RouteModel(t.Context(), requestWithModel(tc.model))
			if err != nil {
				t.Fatal(err)
			}
			if resp.Handled != tc.owned {
				t.Fatalf("model %q: Handled=%v, want %v", tc.model, resp.Handled, tc.owned)
			}
		})
	}
}
