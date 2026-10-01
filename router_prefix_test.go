package plugin

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRouterRequestedModelIsAuthoritative(t *testing.T) {
	router := NewRouter(&pluginConfig{})
	for _, tc := range []struct {
		requested string
		body      string
		owned     bool
	}{
		{"deepseek-flash", "commandcode/deepseek-flash", false},
		{"glm-5.3-flash", "commandcode/z-ai/glm-5.3-flash", false},
		{"deepseek/deepseek-v4.1-flash", "commandcode/deepseek/deepseek-v4.1-flash", false},
		{"opencode-go/deepseek-v4.1-flash", "commandcode/deepseek-flash", false},
		{"commandcode/gpt-5", "commandcode/deepseek-flash", false},
		{"commandcode/deepseek-flash", "opencode-go/deepseek-flash", true},
		{"", "commandcode/deepseek-flash", true},
		{" ", "commandcode/deepseek-flash", true},
		{"", "deepseek-flash", false},
		{"", "opencode-go/deepseek-flash", false},
	} {
		t.Run(tc.requested+"->"+tc.body, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{"model": tc.body})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := router.RouteModel(t.Context(), pluginapi.ModelRouteRequest{
				RequestedModel: tc.requested,
				Body:           body,
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Handled != tc.owned {
				t.Fatalf("requested=%q, body=%q: Handled=%v, want %v", tc.requested, tc.body, resp.Handled, tc.owned)
			}
		})
	}
}
