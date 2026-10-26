package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/testenv"
	"hegel.dev/go/hegel"
)

// Removing teammate decoding from any renderer loses the source attribution.
func TestPropertyEventTeammateDisplay(t *testing.T) {
	for _, surface := range []string{"human", "agent", "tail-agent", "tail-json", "tail-human"} {
		t.Run(surface, func(t *testing.T) {
			check := func(tb testing.TB, handle string) {
				event, err := json.Marshal(map[string]any{"event_id": 17, "type": "issue.closed", "project_name": "example-project", "actor": "alice", "teammate": handle})
				require.NoError(tb, err)
				page := []byte(`{"events":[` + string(event) + `],"next_after_id":17}`)
				var out bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetOut(&out)
				switch surface {
				case "human":
					require.NoError(tb, printEventsHuman(cmd, page))
					want := "by alice"
					if handle != "" {
						want += " / " + handle
					}
					require.Contains(tb, out.String(), want)
				case "agent":
					require.NoError(tb, printEventsAgent(cmd, page))
					require.Contains(tb, out.String(), "actor=alice")
					if handle != "" {
						require.Contains(tb, out.String(), "teammate="+handle)
					} else {
						require.NotContains(tb, out.String(), "teammate=")
					}
				default:
					f := frameState{id: "17", event: "issue.closed", data: string(event)}
					mode := outputJSON
					if surface == "tail-agent" {
						mode = outputAgent
					}
					if surface == "tail-human" {
						mode = outputHuman
					}
					result, err := f.flush(&out, mode)
					require.NoError(tb, err)
					require.Equal(tb, int64(17), result.Progress.lastID)
					if surface == "tail-agent" {
						require.Contains(tb, out.String(), "actor=alice")
						if handle != "" {
							require.Contains(tb, out.String(), "teammate="+handle)
						} else {
							require.NotContains(tb, out.String(), "teammate=")
						}
					} else {
						var got map[string]any
						require.NoError(tb, json.Unmarshal(out.Bytes(), &got))
						require.Equal(tb, handle, got["teammate"])
					}
				}
			}
			for _, h := range []string{"", "claude.opus-5-5", strings.Repeat("Z", 64)} {
				check(t, h)
			}
			hegel.Test(t, func(ht *hegel.T) { check(ht, drawDisplayHandle(ht)) }, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
		})
	}
}

// The display contract accepts the complete teammate grammar, including empty.
func drawDisplayHandle(ht *hegel.T) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-"
	n := hegel.Draw(ht, hegel.Integers(0, 64))
	var b strings.Builder
	for range n {
		alphabet := chars
		if b.Len() == 0 {
			alphabet = chars[:62]
		}
		b.WriteByte(alphabet[hegel.Draw(ht, hegel.Integers(0, len(alphabet)-1))])
	}
	return b.String()
}

func TestPropertyAuditTeammateDisplay(t *testing.T) {
	for _, surface := range []string{"human", "agent"} {
		t.Run(surface, func(t *testing.T) {
			check := func(tb testing.TB, h string) {
				bs, err := json.Marshal(map[string]any{"rows": []any{map[string]any{"actor": "alice", "teammate": h, "issue": "abc4", "reason": "done"}}})
				require.NoError(tb, err)
				var out bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetOut(&out)
				if surface == "human" {
					require.NoError(tb, printAuditClosesTable(cmd, bs))
					require.Contains(tb, out.String(), "TEAMMATE")
					require.Contains(tb, out.String(), "alice")
					if h != "" {
						require.Contains(tb, out.String(), h)
					}
				} else {
					require.NoError(tb, printAuditClosesAgent(cmd, bs))
					require.Contains(tb, out.String(), "actor=alice")
					if h != "" {
						require.Contains(tb, out.String(), "teammate="+h)
					} else {
						require.NotContains(tb, out.String(), "teammate=")
					}
				}
			}
			check(t, "claude")
			check(t, "")
			hegel.Test(t, func(ht *hegel.T) { check(ht, drawDisplayHandle(ht)) }, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
		})
	}
}

func TestPropertyDigestTeammateDisplay(t *testing.T) {
	for _, surface := range []string{"human", "agent"} {
		t.Run(surface, func(t *testing.T) {
			check := func(tb testing.TB, h string) {
				teams := []string{}
				if h != "" {
					teams = append(teams, h)
				}
				bs, err := json.Marshal(map[string]any{"actors": []any{map[string]any{"actor": "alice", "teammates": teams, "issues": []any{map[string]any{"issue_short_id": "abc4", "actions": []string{"created"}}}}}})
				require.NoError(tb, err)
				var out bytes.Buffer
				cmd := &cobra.Command{}
				cmd.SetOut(&out)
				if surface == "human" {
					require.NoError(tb, printDigestHuman(cmd, bs))
					want := "alice"
					if h != "" {
						want += " (" + h + ")"
					}
					require.Contains(tb, out.String(), want+" —")
				} else {
					require.NoError(tb, printDigestAgent(cmd, bs))
					require.Contains(tb, out.String(), "actor=alice")
					if h != "" {
						require.Contains(tb, out.String(), "teammates="+h)
					}
				}
			}
			check(t, "claude")
			check(t, "")
			hegel.Test(t, func(ht *hegel.T) { check(ht, drawDisplayHandle(ht)) }, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
		})
	}
}

func TestTeammateDisplayCLIEndToEnd(t *testing.T) {
	env := testenv.New(t)
	dir, _ := initLocalBoundWorkspace(t, env, "example-project")
	ref := runCLI(t, env, dir, "--as", "alice", "--teammate", "codex", "--quiet", "create", "Display task")
	runCLI(t, env, dir, "--as", "alice", "--teammate", "claude", "comment", ref, "--body", "Reviewed display behavior.")
	runCLI(t, env, dir, "--as", "alice", "--teammate", "codex", "close", ref, "--done", "--message", "Completed display task with focused behavior checks.", "--evidence", "test:go test ./example")
	for _, args := range [][]string{{"events"}, {"events", "--agent"}, {"events", "--json"}, {"audit", "closes"}, {"audit", "closes", "--agent"}, {"audit", "closes", "--json"}, {"digest", "--since", "1h"}, {"digest", "--since", "1h", "--agent"}, {"digest", "--since", "1h", "--json"}, {"show", ref}, {"show", ref, "--agent"}, {"show", ref, "--json"}} {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			out := runCLI(t, env, dir, args...)
			switch args[0] {
			case "show":
				if len(args) == 2 {
					require.Contains(t, out, "closed: alice / codex (done)")
				} else if args[2] == "--agent" {
					require.Contains(t, out, `Closed: "alice / codex (done)"`)
				} else {
					var got struct {
						Close struct {
							Actor    string
							Teammate string
						}
					}
					require.NoError(t, json.Unmarshal([]byte(out), &got))
					require.Equal(t, "alice", got.Close.Actor)
					require.Equal(t, "codex", got.Close.Teammate)
				}
			case "events":
				if len(args) == 1 {
					require.Contains(t, out, "alice / codex")
				} else if args[1] == "--agent" {
					require.Contains(t, out, "actor=alice teammate=codex")
				} else {
					require.Contains(t, out, `"teammate":"codex"`)
				}
			case "audit":
				require.Contains(t, out, "codex")
				if len(args) == 2 {
					require.Contains(t, out, "TEAMMATE")
				} else if args[2] == "--agent" {
					require.Contains(t, out, "teammate=codex")
				}
			case "digest":
				if len(args) == 3 {
					require.Contains(t, out, "alice (claude, codex)")
				} else if args[3] == "--agent" {
					require.Contains(t, out, "teammates=claude,codex")
				} else {
					var got struct {
						Actors []struct {
							Actor     string
							Teammates []string
						}
					}
					require.NoError(t, json.Unmarshal([]byte(out), &got))
					require.Len(t, got.Actors, 1)
					require.Equal(t, []string{"claude", "codex"}, got.Actors[0].Teammates)
				}
			}
		})
	}
}

func TestPropertyShowCloseTeammateDisplay(t *testing.T) {
	for _, surface := range []string{"human", "agent"} {
		t.Run(surface, func(t *testing.T) {
			check := func(tb testing.TB, h string) {
				raw, err := json.Marshal(map[string]any{"issue": map[string]any{"short_id": "abc4", "status": "closed", "author": "creator"}, "close": map[string]any{"actor": "alice", "teammate": h, "reason": "done", "event_id": 17}})
				require.NoError(tb, err)
				var b showResponseForCLI
				require.NoError(tb, json.Unmarshal(raw, &b))
				var out bytes.Buffer
				if surface == "human" {
					require.NoError(tb, printShowHuman(&out, b, "example-project", nil))
				} else {
					require.NoError(tb, printShowAgent(&out, b, "example-project", "show"))
				}
				want := "alice"
				if h != "" {
					want += " / " + h
				}
				require.Contains(tb, out.String(), want)
			}
			check(t, "claude")
			check(t, "")
			hegel.Test(t, func(ht *hegel.T) { check(ht, drawDisplayHandle(ht)) }, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
		})
	}
}

func TestEventTeammateTailEndToEnd(t *testing.T) {
	for _, mode := range []string{"human", "agent", "json"} {
		t.Run(mode, func(t *testing.T) {
			env := testenv.New(t)
			dir, pid := initLocalBoundWorkspace(t, env, "example-project")
			before, err := env.DB.MaxEventID(t.Context())
			require.NoError(t, err)
			_, _, err = env.DB.CreateIssue(t.Context(), db.CreateIssueParams{ProjectID: pid, Title: "Tail task", Author: "alice", Metadata: map[string]jsontext.Value{"teammate": jsontext.Value(`"claude"`)}})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(contextWithBaseURL(context.Background(), env.URL), 5*time.Second)
			defer cancel()
			args := []string{"--workspace", dir, "events", "--tail", "--last-event-id", fmt.Sprint(before)}
			if mode != "human" {
				args = append(args, "--"+mode)
			}
			tail := startAsyncCLI(t, ctx, args...)
			defer tail.stop()
			out := tail.awaitOutput(func(s string) bool { return strings.Contains(s, "issue.created") }, 2*time.Second)
			if mode == "agent" {
				require.Contains(t, out, "actor=alice teammate=claude")
			} else {
				var got struct {
					Actor    string
					Teammate string
				}
				require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &got))
				require.Equal(t, "alice", got.Actor)
				require.Equal(t, "claude", got.Teammate)
			}
		})
	}
}
