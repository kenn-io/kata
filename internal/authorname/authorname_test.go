package authorname

import "testing"

func TestDisplay(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	tests := []struct{ input, want string }{
		{"Ana [member:" + id + "; enrollment:" + id + "]", "Ana"},
		{"Bot [service-account:" + id + "; token:" + id + "]", "Bot"},
		{"Ana", "Ana"},
		{"Ana [team]", "Ana [team]"},
		{"Ana [member:" + id + "; token:" + id + "]", "Ana [member:" + id + "; token:" + id + "]"},
		{"Ana [member:short; enrollment:" + id + "]", "Ana [member:short; enrollment:" + id + "]"},
		{"Ana [member:" + id + "; enrollment:" + id + "]\n", "Ana [member:" + id + "; enrollment:" + id + "]\n"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := Display(tt.input); got != tt.want {
				t.Errorf("Display(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
