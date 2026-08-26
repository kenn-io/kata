package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOutputModePrecedenceTable pins the user-visible --format/--json/--agent
// contract: how repeated and mixed selections resolve, when they conflict,
// which commands allow contract output, and how the import command's legacy
// --format kata|beads overload is skipped rather than rejected.
func TestOutputModePrecedenceTable(t *testing.T) {
	cases := []struct {
		name            string
		formats         []string
		json            bool
		agent           bool
		importLegacy    bool
		contractAllowed bool
		want            outputMode
		wantErr         string
	}{
		{name: "no selection is human", want: outputHuman},
		{name: "--json", json: true, want: outputJSON},
		{name: "--agent", agent: true, want: outputAgent},
		{name: "--format human", formats: []string{"human"}, want: outputHuman},
		{name: "--format json", formats: []string{"json"}, want: outputJSON},
		{name: "--format agent", formats: []string{"agent"}, want: outputAgent},
		{name: "repeated --format agreeing", formats: []string{"json", "json"}, want: outputJSON},
		{name: "repeated --format disagreeing", formats: []string{"human", "json"},
			wantErr: "conflicting output modes"},
		{name: "--format json with --agent", formats: []string{"json"}, agent: true,
			wantErr: "conflicting output modes"},
		{name: "--format human with --json", formats: []string{"human"}, json: true,
			wantErr: "conflicting output modes"},
		{name: "--format agent with --agent agrees", formats: []string{"agent"}, agent: true,
			want: outputAgent},
		{name: "--json with --agent", json: true, agent: true,
			wantErr: "conflicting output modes"},
		{name: "empty format value is skipped", formats: []string{""}, want: outputHuman},
		{name: "empty format value does not conflict", formats: []string{"", "json"}, want: outputJSON},
		{name: "unsupported format", formats: []string{"xml"},
			wantErr: `unsupported output format "xml"`},
		{name: "contract allowed", formats: []string{"contract"}, contractAllowed: true,
			want: outputContract},
		{name: "contract disallowed", formats: []string{"contract"},
			wantErr: `unsupported output format "contract"`},
		{name: "contract allowed with --json", formats: []string{"contract"}, contractAllowed: true,
			json: true, wantErr: "conflicting output modes"},
		{name: "contract allowed with --agent", formats: []string{"contract"}, contractAllowed: true,
			agent: true, wantErr: "conflicting output modes"},
		{name: "import legacy beads is skipped", formats: []string{"beads"}, importLegacy: true,
			want: outputHuman},
		{name: "import legacy kata is skipped", formats: []string{"kata"}, importLegacy: true,
			want: outputHuman},
		{name: "import legacy beads with --json still json", formats: []string{"beads"},
			importLegacy: true, json: true, want: outputJSON},
		{name: "beads outside import is unsupported", formats: []string{"beads"},
			wantErr: `unsupported output format "beads"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel := outputSelection{formats: tc.formats, json: tc.json, agent: tc.agent}
			got, err := sel.resolve(tc.importLegacy, tc.contractAllowed)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				var cli *cliError
				require.ErrorAs(t, err, &cli)
				assert.Equal(t, ExitUsage, cli.ExitCode, "a bad output selection is a usage error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestOutputSelectionResolveFallbackMode pins the mode resolve returns
// alongside an error. That mode is what emitRootError renders the failure in,
// so an --agent caller whose other flag was bad still gets a parseable ERR
// line rather than human prose.
func TestOutputSelectionResolveFallbackMode(t *testing.T) {
	cases := []struct {
		name            string
		sel             outputSelection
		contractAllowed bool
		want            outputMode
	}{
		{
			name: "unsupported format alongside --agent falls back to agent",
			sel:  outputSelection{formats: []string{"xml"}, agent: true},
			want: outputAgent,
		},
		{
			name: "unsupported format alongside --json falls back to json",
			sel:  outputSelection{formats: []string{"xml"}, json: true},
			want: outputJSON,
		},
		{
			name: "unsupported format alone falls back to human",
			sel:  outputSelection{formats: []string{"xml"}},
			want: outputHuman,
		},
		{
			name: "conflicting selection falls back to human",
			sel:  outputSelection{formats: []string{"json"}, agent: true},
			want: outputHuman,
		},
		{
			name:            "contract conflict with --agent falls back to agent",
			sel:             outputSelection{formats: []string{"contract"}, agent: true},
			contractAllowed: true,
			want:            outputAgent,
		},
		{
			name:            "contract conflict with json format falls back to json",
			sel:             outputSelection{formats: []string{"contract", "json"}},
			contractAllowed: true,
			want:            outputJSON,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.sel.resolve(false, tc.contractAllowed)
			require.Error(t, err)
			assert.Equal(t, tc.want, got, "resolve must return a usable mode with its error")
		})
	}
}

// errorPathArgvCases are the argv shapes whose command selection and resolved
// output mode the error path must reproduce. -q is the shorthand case where
// the former argv walkers disagreed: one resolved `import` while the other
// abandoned command resolution at the shorthand and stayed on root.
var errorPathArgvCases = []struct {
	name        string
	args        []string
	wantCommand string
	wantSel     outputSelection
	wantMode    outputMode
}{
	{
		name: "shorthand before import with legacy format",
		args: []string{"-q", "import", "--format", "beads"},
		// beads is the legacy import-source value, skipped for mode selection.
		wantCommand: "import",
		wantSel:     outputSelection{formats: []string{"beads"}},
		wantMode:    outputHuman,
	},
	{
		name:        "root string flag with value then subcommand flag",
		args:        []string{"--workspace", "/tmp/example-workspace", "list", "--json"},
		wantCommand: "list",
		wantSel:     outputSelection{json: true},
		wantMode:    outputJSON,
	},
	{
		name: "flags after the -- separator are positional",
		args: []string{"list", "--", "--json"},
		// --json after -- is a positional, so it must not select JSON.
		wantCommand: "list",
		wantSel:     outputSelection{},
		wantMode:    outputHuman,
	},
	{
		name:        "attached long-flag value before the command",
		args:        []string{"--format=agent", "show", "abc4"},
		wantCommand: "show",
		wantSel:     outputSelection{formats: []string{"agent"}},
		wantMode:    outputAgent,
	},
	{
		name:        "quickstart contract format",
		args:        []string{"quickstart", "--format", "contract"},
		wantCommand: "quickstart",
		wantSel:     outputSelection{formats: []string{"contract"}},
		wantMode:    outputContract,
	},
}

// TestErrorPathArgvSelectsCommand pins which command each argv shape selects
// and which output flags it carries. The command decides importLegacy and
// contractAllowed, which decide whether overloaded --format values are valid.
func TestErrorPathArgvSelectsCommand(t *testing.T) {
	for _, tc := range errorPathArgvCases {
		t.Run(tc.name, func(t *testing.T) {
			root := newRootCmd()
			cmd, sel := preParse(root, tc.args)
			require.NotNil(t, cmd)
			assert.Equal(t, tc.wantCommand, commandLeaf(cmd))
			assert.Equal(t, tc.wantSel, sel)
		})
	}
}

// TestPreParseIsTotal covers the degenerate inputs the error path can hand
// preParse: it must answer without panicking, since it runs after cobra has
// already failed.
func TestPreParseIsTotal(t *testing.T) {
	root := newRootCmd()
	for _, args := range [][]string{
		nil,
		{},
		{"--"},
		{"--format"},                       // trailing flag with no value
		{"--json=notabool"},                // unparseable bool value
		{"-1"},                             // negative-number positional
		{"nonexistent", "--agent"},         // unknown command, flag still counts
		{"show", "abc4", "def5", "--json"}, // extra positionals do not stop flags
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			cmd, sel := preParse(root, args)
			assert.NotNil(t, cmd)
			mode, _ := sel.resolve(isImportCommand(cmd), supportsContractOutput(cmd))
			assert.Contains(t, []outputMode{outputHuman, outputJSON, outputAgent, outputContract}, mode)
		})
	}
	cmd, sel := preParse(root, []string{"nonexistent", "--agent"})
	assert.Same(t, root, cmd, "an unknown command leaves the root selected")
	assert.True(t, sel.agent, "flags after an unknown positional still count")

	cmd, sel = preParse(root, []string{"show", "abc4", "def5", "--json"})
	assert.Equal(t, "show", commandLeaf(cmd))
	assert.True(t, sel.json, "extra positionals stop command resolution, not flag scanning")

	_, sel = preParse(root, []string{"--json=notabool"})
	assert.False(t, sel.json, "an unparseable --json value leaves the selection untouched")
}

// TestErrorPathArgvResolvesMode pins the end-to-end error-path answer: the mode
// emitRootError renders in when cobra failed before PersistentPreRunE could
// resolve one.
func TestErrorPathArgvResolvesMode(t *testing.T) {
	for _, tc := range errorPathArgvCases {
		t.Run(tc.name, func(t *testing.T) {
			resetFlags(t)
			root := newRootCmd()
			got, err := resolvedOutputModeForError(root, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.wantMode, got)
		})
	}
}

func TestAgentValue_Quoting(t *testing.T) {
	assert.Equal(t, "abc4", agentValue("abc4"))
	assert.Equal(t, strconv.Quote("Fix login race"), agentValue("Fix login race"))
	assert.Equal(t, strconv.Quote(`quoted "title"`), agentValue(`quoted "title"`))
	assert.Equal(t, strconv.Quote("bad\nline"), agentValue("bad\nline"))
}

func TestAgentFencedText_ExtendsFenceForBackticks(t *testing.T) {
	got := agentFencedText("``` inside")
	assert.Contains(t, got, "````text\n")
	assert.True(t, strings.HasSuffix(got, "\n````\n"))
}

func TestAgentFencedText_ChoosesFenceAfterSanitizing(t *testing.T) {
	got := agentFencedText("``\x00` inside")
	assert.Contains(t, got, "````text\n")
	assert.Contains(t, got, "``` inside")
	assert.True(t, strings.HasSuffix(got, "\n````\n"))
}
