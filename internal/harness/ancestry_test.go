package harness

import (
	"errors"
	"fmt"
	"testing"
)

type processFixture struct {
	name    string
	args    []string
	ppid    int32
	created int64
	err     error
}

func fixtureAncestry(fixtures ...processFixture) Ancestry {
	return processAncestry{
		startPID: 100,
		lookup: func(pid int32) (string, []string, int32, int64, error) {
			i := int(pid - 100)
			if i < 0 || i >= len(fixtures) {
				return "", nil, 0, 0, errors.New("missing fixture")
			}
			f := fixtures[i]
			return f.name, f.args, f.ppid, f.created, f.err
		},
	}
}

func TestProcessAncestrySkipsShells(t *testing.T) {
	for _, tt := range []struct {
		name                        string
		processes                   []processFixture
		wantHarness, wantGeneration string
		wantOK                      bool
	}{
		{"kata zsh codex", []processFixture{{name: "kata", args: []string{"kata", "agent-hook", "start", "muse"}, ppid: 101}, {name: "zsh", ppid: 102}, {name: "codex", created: 123456}}, "codex", "102:123456", true},
		{"kata zsh terminal", []processFixture{{name: "kata", args: []string{"kata", "agent-hook", "start", "muse"}, ppid: 101}, {name: "zsh", ppid: 102}, {name: "Terminal", ppid: 103}, {name: "codex", created: 123456}}, "", "", false},
		{"daemon barrier", []processFixture{{name: "bash", ppid: 101}, {name: "kata-daemon", ppid: 102}, {name: "codex", created: 123456}}, "", "", false},
		{"daemon launcher barrier", []processFixture{{name: "kata", args: []string{"kata", "daemon", "start", "--foreground"}, ppid: 101}, {name: "codex", created: 123456}}, "", "", false},
		{"daemon with root flags", []processFixture{{name: "kata", args: []string{"kata", "--format", "json", "daemon", "start", "--foreground"}, ppid: 101}, {name: "codex", created: 123456}}, "", "", false},
		{"Windows daemon barrier", []processFixture{{name: `C:\tools\KATA.EXE`, args: []string{`C:\tools\KATA.EXE`, "daemon", "start", "--foreground"}, ppid: 101}, {name: "codex", created: 123456}}, "", "", false},
		{"node pi", []processFixture{{name: "node", args: []string{"node", "/x/bin/pi"}, created: 123456}}, "pi", "100:123456", true},
		{"bun pi", []processFixture{{name: "bun", args: []string{"bun", "/x/bin/pi"}, created: 123456}}, "pi", "100:123456", true},
		{"Windows node pi", []processFixture{{name: `C:\tools\node.EXE`, args: []string{`C:\tools\node.EXE`, `C:\tools\bin\pi`}, created: 123456}}, "pi", "100:123456", true},
		{"unrelated node script", []processFixture{{name: "node", args: []string{"node", "/x/pi/other"}, ppid: 101}, {name: "pi", created: 123456}}, "", "", false},
		{"unrelated node cli.js", []processFixture{{name: "node", args: []string{"node", "/x/node_modules/unrelated-package/dist/cli.js"}, created: 123456}}, "", "", false},
		{"pi elsewhere in argv", []processFixture{{name: "node", args: []string{"node", "other", "/x/bin/pi"}, created: 123456}}, "", "", false},
		{"runtime missing argv", []processFixture{{name: "bun", created: 123456}}, "", "", false},
		{"noncandidate barrier", []processFixture{{name: "omp", ppid: 101, created: 123456}, {name: "codex", created: 123456}}, "", "", false},
		{"nearest candidate", []processFixture{{name: "pi", ppid: 101, created: 123456}, {name: "codex", created: 654321}}, "pi", "100:123456", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, generation, ok := fixtureAncestry(tt.processes...).Nearest([]string{"codex", "pi"})
			if got != tt.wantHarness || generation != tt.wantGeneration || ok != tt.wantOK {
				t.Fatalf("Nearest() = (%q, %q, %v), want (%q, %q, %v)", got, generation, ok, tt.wantHarness, tt.wantGeneration, tt.wantOK)
			}
		})
	}
}

func TestProcessAncestryExecutableTable(t *testing.T) {
	for _, tt := range []struct{ name, harness string }{
		{"claude", "claude"}, {"codex", "codex"}, {"pi", "pi"}, {"omp", "omp"},
		{"muse", "muse"}, {"muse-bin-1.2.3", "muse"}, {"amp", "amp"},
		{"cursor-agent", "cursor"}, {"openclaw", "openclaw"}, {"gemini", "gemini"}, {"qwen", "qwen"},
		{"/x/bin/CODEX.EXE", "codex"},
		{`C:\tools\CODEX.EXE`, "codex"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, generation, ok := fixtureAncestry(processFixture{name: tt.name, created: 987654321}).Nearest([]string{tt.harness})
			if !ok || got != tt.harness || generation != "100:987654321" {
				t.Fatalf("Nearest() = (%q, %q, %v), want (%q, 100:987654321, true)", got, generation, ok, tt.harness)
			}
		})
	}
}

// Persistent root flags can appear between daemon and start. Their position
// must never let a daemon descendant inherit the launching agent's identity.
func TestProcessAncestryDaemonCommandFlags(t *testing.T) {
	for _, tt := range []struct {
		name   string
		args   []string
		wantOK bool
	}{
		{"format between subcommands", []string{"kata", "daemon", "--format", "json", "start", "--foreground"}, false},
		{"Windows format between subcommands", []string{`C:\tools\KATA.EXE`, "daemon", "--format", "json", "start", "--foreground"}, false},
		{"bool between subcommands", []string{"kata", "daemon", "--json", "start", "--foreground"}, false},
		{"equals flag between subcommands", []string{"kata", "daemon", "--format=json", "start", "--foreground"}, false},
		{"root and intervening flags", []string{"kata", "--as", "example-actor", "--workspace", "/x/workspace", "daemon", "--format", "json", "start", "--foreground"}, false},
		{"root string value named start", []string{"kata", "--teammate", "start", "daemon", "--format", "json", "start", "--foreground"}, false},
		{"ordinary wrapper with daemon flag value", []string{"kata", "--teammate", "daemon", "agent-hook", "start", "muse"}, true},
		{"ordinary wrapper with equals flag", []string{"kata", "--teammate=daemon", "agent-hook", "start", "muse"}, true},
		{"ordinary search argument text", []string{"kata", "search", "daemon", "start"}, true},
		{"ordinary wrapper with short boolean flag", []string{"kata", "-q", "agent-hook", "start", "muse"}, true},
		{"ordinary wrapper with bool equals flag", []string{"kata", "--json=false", "agent-hook", "start", "muse"}, true},
		{"ordinary wrapper with root string flags", []string{"kata", "--format", "json", "--as", "daemon", "--project", "example-project", "--workspace", "/x/workspace", "--daemon", "example-daemon", "agent-hook", "start", "muse"}, true},
		{"unknown root flag", []string{"kata", "--unknown", "value", "agent-hook", "start", "muse"}, false},
		{"invalid boolean flag value", []string{"kata", "--json=unknown", "agent-hook", "start", "muse"}, false},
		{"missing root string value", []string{"kata", "--workspace"}, false},
		{"missing root command", []string{"kata", "--json"}, false},
		{"missing argv", []string{"kata"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probe := fixtureAncestry(processFixture{name: tt.args[0], args: tt.args, ppid: 101}, processFixture{name: "codex", created: 123456})
			got, generation, ok := probe.Nearest([]string{"codex"})
			wantHarness, wantGeneration := "", ""
			if tt.wantOK {
				wantHarness, wantGeneration = "codex", "101:123456"
			}
			if got != wantHarness || generation != wantGeneration || ok != tt.wantOK {
				t.Fatalf("Nearest() = (%q, %q, %v), want (%q, %q, %v)", got, generation, ok, wantHarness, wantGeneration, tt.wantOK)
			}
		})
	}
}

func TestProcessAncestryWrapperTable(t *testing.T) {
	for _, name := range []string{"sh", "bash", "zsh", "dash", "fish", "cmd", "powershell", "pwsh", "env", "kata", "/x/bin/BASH.EXE"} {
		t.Run(name, func(t *testing.T) {
			var args []string
			if name == "kata" {
				args = []string{"kata", "agent-hook", "start", "muse"}
			}
			got, generation, ok := fixtureAncestry(processFixture{name: name, args: args, ppid: 101}, processFixture{name: "codex", created: 123456}).Nearest([]string{"codex"})
			if !ok || got != "codex" || generation != "101:123456" {
				t.Fatalf("Nearest() = (%q, %q, %v), want (codex, 101:123456, true)", got, generation, ok)
			}
		})
	}
}

func TestProcessAncestryRejectsUnavailableOrFalseMatches(t *testing.T) {
	for _, fixture := range []processFixture{
		{name: "codex", created: 123456, err: errors.New("lookup denied")},
		{name: "codex", created: 0}, {name: "codex", created: -1},
		{name: "codex-helper", created: 123456}, {name: "muse-bin-", created: 123456},
		{name: "other-muse-bin-1.2.3", created: 123456},
		{name: "node", args: []string{"node", "/x/bin/pi-other"}, created: 123456},
	} {
		t.Run(fmt.Sprintf("%s-%d-%v", fixture.name, fixture.created, fixture.args), func(t *testing.T) {
			got, generation, ok := fixtureAncestry(fixture).Nearest([]string{"codex", "muse", "pi"})
			if ok || got != "" || generation != "" {
				t.Fatalf("Nearest() = (%q, %q, %v), want empty false", got, generation, ok)
			}
		})
	}
}

func TestProcessAncestryBoundedWalk(t *testing.T) {
	for _, depth := range []int{1, 15, 16, 17} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			calls := 0
			probe := processAncestry{startPID: 100, lookup: func(pid int32) (string, []string, int32, int64, error) {
				calls++
				if int(pid-100)+1 == depth {
					return "codex", nil, 1, 123456, nil
				}
				return "zsh", nil, pid + 1, 0, nil
			}}
			got, generation, ok := probe.Nearest([]string{"codex"})
			if depth <= 16 {
				wantGeneration := fmt.Sprintf("%d:123456", 100+depth-1)
				if !ok || got != "codex" || generation != wantGeneration || calls != depth {
					t.Fatalf("depth %d = (%q, %q, %v), calls %d", depth, got, generation, ok, calls)
				}
			} else if ok || got != "" || generation != "" || calls != 16 {
				t.Fatalf("depth limit = (%q, %q, %v), calls %d; want empty false after 16", got, generation, ok, calls)
			}
		})
	}
}

func TestProcessAncestryRejectsInvalidPIDAndCycles(t *testing.T) {
	for _, start := range []int32{-1, 0, 1, 100} {
		calls := 0
		probe := processAncestry{startPID: start, lookup: func(pid int32) (string, []string, int32, int64, error) {
			calls++
			return "zsh", nil, pid, 0, nil
		}}
		got, generation, ok := probe.Nearest([]string{"codex"})
		if ok || got != "" || generation != "" || calls > 16 || (start <= 1 && calls != 0) {
			t.Fatalf("PID %d = (%q, %q, %v), calls %d; want bounded empty false", start, got, generation, ok, calls)
		}
	}
}
