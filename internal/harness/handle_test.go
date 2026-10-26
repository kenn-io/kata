package harness

import (
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	for _, want := range []Mode{ModeModel, ModeHarness, ModeOff} {
		t.Run(string(want), func(t *testing.T) {
			got, err := ParseMode(string(want))
			if err != nil || got != want {
				t.Fatalf("ParseMode(%q) = %q, %v; want %q, nil", want, got, err, want)
			}
		})
	}
	for _, input := range []string{"", "MODEL", " model", "model ", "auto", "disabled", "harness\x00"} {
		t.Run("reject_"+input, func(t *testing.T) {
			if got, err := ParseMode(input); err == nil {
				t.Fatalf("ParseMode(%q) = %q, nil; want validation error", input, got)
			}
		})
	}
}

func TestHandleExamples(t *testing.T) {
	tests := []struct {
		name string
		d    Detection
		want string
	}{
		{"provider", Detection{Harness: "pi", Provider: "zai", Model: "glm-5.3"}, "pi.zai-glm-5.3"},
		{"claude", Detection{Harness: "claude", Model: "claude-opus-5-5[1m]"}, "claude.opus-5-5-1m"},
		{"codex", Detection{Harness: "codex", Model: "gpt-5.1"}, "codex.gpt-5.1"},
		{"muse", Detection{Harness: "muse", Model: "muse-spark"}, "muse.spark"},
		{"router", Detection{Harness: "pi", Provider: "openrouter", Model: "z-ai/glm-4.6"}, "pi.openrouter-z-ai-glm-4.6"},
		{"existing_provider", Detection{Harness: "pi", Provider: "zai", Model: "zai-glm-5.3"}, "pi.zai-glm-5.3"},
		{"provider_case", Detection{Harness: "pi", Provider: "ZAI", Model: "Zai-glm-5.3"}, "pi.zai-glm-5.3"},
		{"harness_case", Detection{Harness: "claude", Model: "CLAUDE-OPUS-5-5[1M]"}, "claude.opus-5-5-1m"},
		{"leading_prefix_only", Detection{Harness: "muse", Model: "spark-muse-spark"}, "muse.spark-muse-spark"},
		{"single_prefix", Detection{Harness: "muse", Model: "muse-muse-spark"}, "muse.muse-spark"},
		{"slug", Detection{Harness: "pi", Model: "._-GPT /\x00\t✨ 5.1-_."}, "pi.gpt-5.1"},
		{"preserve_separators", Detection{Harness: "pi", Model: "a._--b"}, "pi.a._--b"},
		{"unicode", Detection{Harness: "pi", Model: "é漢✨\x00GPT\nλ"}, "pi.gpt"},
		{"invalid_utf8", Detection{Harness: "pi", Model: "a\xff\xfeb"}, "pi.a-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Handle(tt.d, ModeModel); got != tt.want {
				t.Fatalf("Handle(%+v, model) = %q; want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestHandleEmptyModelPartFallsBack(t *testing.T) {
	for _, model := range []string{"", "???", "[]", "-", "._-", "\x00✨", "pi-"} {
		t.Run(model, func(t *testing.T) {
			if got := Handle(Detection{Harness: "pi", Model: model}, ModeModel); got != "pi" {
				t.Fatalf("Handle(model=%q) = %q; want pi", model, got)
			}
		})
	}
}

func TestHandleUnknownModelWithProvider(t *testing.T) {
	for _, model := range []string{"", "???", "[]", "-", "._-", "\x00✨"} {
		t.Run(model, func(t *testing.T) {
			if got := Handle(Detection{Harness: "pi", Provider: "zai", Model: model}, ModeModel); got != "pi" {
				t.Fatalf("Handle(provider=zai, model=%q) = %q; want pi", model, got)
			}
		})
	}
}

func TestHandleModes(t *testing.T) {
	d := Detection{Harness: "pi", Provider: "zai", Model: "glm-5.3"}
	if got := Handle(d, ModeHarness); got != "pi" {
		t.Fatalf("Handle(harness) = %q; want pi", got)
	}
	if got := Handle(d, ModeOff); got != "" {
		t.Fatalf("Handle(off) = %q; want empty", got)
	}
	for _, mode := range []Mode{ModeModel, ModeHarness, ModeOff} {
		if got := Handle(Detection{Model: "gpt-5.1"}, mode); got != "" {
			t.Fatalf("Handle(no harness, %q) = %q; want empty", mode, got)
		}
	}
}

func TestHandleTruncation(t *testing.T) {
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			partLength := 64 - len(name) - 1
			for _, length := range []int{partLength - 1, partLength, partLength + 1, 1024} {
				model := strings.Repeat("x", length)
				want := name + "." + strings.Repeat("x", min(length, partLength))
				if got := Handle(Detection{Harness: name, Model: model}, ModeModel); got != want {
					t.Fatalf("Handle(%q, model length %d) = %q; want %q", name, length, got, want)
				}
			}
		})
	}
}
