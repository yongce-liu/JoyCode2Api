package joycode

import "testing"

func TestResolveModel(t *testing.T) {
	for _, tc := range []struct {
		name           string
		model          string
		accountDefault string
		systemDefault  string
		want           string
	}{
		{"catalog model passes through", "GLM-5.3", "", "", "GLM-5.3"},
		{"id casing is preserved", "gpt-5.6-sol", "", "", "gpt-5.6-sol"},
		{"id with spaces is preserved", "GPT-5.6 Sol", "", "", "GPT-5.6 Sol"},
		{"unknown id is not rewritten", "no-such-model", "", "", "no-such-model"},
		{"unknown id beats configured defaults", "no-such-model", "Kimi-K3", "GLM-5.3", "no-such-model"},
		{"empty uses account default", "", "Kimi-K3", "GLM-5.3", "Kimi-K3"},
		{"empty falls back to system default", "", "", "GLM-5.3", "GLM-5.3"},
		{"empty with no defaults uses the global default", "", "", "", DefaultModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveModel(tc.model, tc.accountDefault, tc.systemDefault)
			if got != tc.want {
				t.Errorf("ResolveModel(%q, %q, %q) = %q, want %q",
					tc.model, tc.accountDefault, tc.systemDefault, got, tc.want)
			}
		})
	}
}
