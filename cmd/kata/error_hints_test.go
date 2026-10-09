package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/testenv"
)

const projectInitHint = `run "kata init --project <name>" in the repository root, or pass --project <name>`

func TestErrorHintsUnboundWorkspace(t *testing.T) {
	setupKataEnv(t)
	t.Setenv("KATA_SERVER", "")
	env := testenv.New(t)
	workspace := t.TempDir()
	for _, mode := range []string{"human", "agent", "json"} {
		t.Run(mode, func(t *testing.T) {
			stdout, stderr, err := executeRootCapture(t, contextWithBaseURL(context.Background(), env.URL),
				"--workspace", workspace, "list", "--format", mode)
			require.Error(t, err)
			assert.Equal(t, ExitNotFound, exitCodeForErr(err, runEEntered))
			assert.Empty(t, stdout)
			switch mode {
			case "human":
				assert.Contains(t, stderr, "\nhint: "+projectInitHint+"\n")
			case "agent":
				assert.Contains(t, stderr, "\nHint: "+projectInitHint+"\n")
				assert.True(t, strings.HasPrefix(stderr, "ERR list not_found:"))
			case "json":
				var got struct {
					Error struct {
						Hint     string `json:"hint"`
						Code     string `json:"code"`
						ExitCode int    `json:"exit_code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(stderr), &got))
				assert.Equal(t, projectInitHint, got.Error.Hint)
				assert.Equal(t, "project_not_initialized", got.Error.Code)
				assert.Equal(t, ExitNotFound, got.Error.ExitCode)
			}
		})
	}
}

func TestErrorHintsPathFreeResolution(t *testing.T) {
	ctx := context.WithValue(context.Background(), pathFreeProjectContextKey{}, true)
	_, _, err := buildPathResolveRequest(ctx, t.TempDir())
	cli := requireCLIError(t, err, ExitNotFound)
	assert.Equal(t, "project_not_initialized", cli.Code)
	assert.Equal(t, "no .kata.toml ancestor and no git ancestor", cli.Message)
	assert.Equal(t, projectInitHint, cli.Hint)
}

func TestErrorHintsDecodeOptionalField(t *testing.T) {
	for _, tc := range []struct{ name, field, want string }{
		{"present", `,"hint":"try again"`, "try again"},
		{"absent", "", ""},
		{"empty", `,"hint":""`, ""},
		{"null", `,"hint":null`, ""},
		{"number", `,"hint":42`, ""},
		{"object", `,"hint":{"next":"retry"}`, ""},
		{"array", `,"hint":["retry"]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"error":{"code":"idempotency_mismatch","message":"same key, different fields","data":{"qualified_id":"example-project#abc4"}` + tc.field + `}}`
			cli := apiErrFromBody(http.StatusConflict, []byte(body))
			var out bytes.Buffer
			emitErrorForMode(&out, cli, outputJSON, true)
			var got struct {
				Error map[string]json.RawMessage `json:"error"`
			}
			require.NoError(t, json.Unmarshal(out.Bytes(), &got))
			var message, hint, code string
			require.NoError(t, json.Unmarshal(got.Error["message"], &message))
			require.NoError(t, json.Unmarshal(got.Error["code"], &code))
			assert.Equal(t, "same key, different fields", message)
			assert.Equal(t, "idempotency_mismatch", code)
			if tc.want == "" {
				assert.NotContains(t, got.Error, "hint")
			} else {
				require.NoError(t, json.Unmarshal(got.Error["hint"], &hint))
				assert.Equal(t, tc.want, hint)
			}
		})
	}
	cli := apiErrFromBody(http.StatusBadRequest, []byte("not JSON"))
	assert.Equal(t, "not JSON", cli.Message)
	assert.Empty(t, cli.Code)
	assert.Equal(t, ExitValidation, cli.ExitCode)
}

func TestErrorHintsOutputAndOmission(t *testing.T) {
	for _, hint := range []string{"", "try again"} {
		for _, mode := range []outputMode{outputHuman, outputAgent, outputJSON} {
			t.Run(fmt.Sprintf("%s/%s", mode, hint), func(t *testing.T) {
				field := ""
				if hint != "" {
					field = `,"hint":"` + hint + `"`
				}
				body := `{"error":{"code":"idempotency_mismatch","message":"conflict","data":{"qualified_id":"example-project#abc4"}` + field + `}}`
				cli := apiErrFromBody(http.StatusConflict, []byte(body))
				var out bytes.Buffer
				emitErrorForMode(&out, cli, mode, true)
				text := out.String()
				if hint == "" {
					assert.NotContains(t, text, `"hint"`)
					assert.NotContains(t, text, "Hint:")
					assert.NotContains(t, text, "hint:")
				} else {
					switch mode {
					case outputHuman:
						assert.Contains(t, text, "\nhint: try again\n")
					case outputAgent:
						assert.Contains(t, text, "\nHint: try again\n")
					case outputJSON:
						assert.Contains(t, text, `"hint":"try again"`)
					}
				}
				if mode == outputAgent {
					assert.Contains(t, text, "example-project#abc4")
				}
			})
		}
	}
}

func TestErrorHintsAgentKeepsFollowupOnOneLine(t *testing.T) {
	cli := apiErrFromBody(http.StatusNotFound, []byte(`{"error":{"code":"project_not_initialized","message":"missing\nextra message","hint":"try again\nextra hint"}}`))
	var out bytes.Buffer
	emitAgentError(&out, "list", cli)
	assert.Equal(t, "ERR list not_found: missing\nHint: try again\n", out.String())
}
