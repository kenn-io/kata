package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/harness"
	"hegel.dev/go/hegel"
)

func teammateTestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("teammate", "", "")
	return cmd
}

func fakeTeammateDetection(t *testing.T, d harness.Detection, outcome harness.Outcome) {
	t.Helper()
	previous := detectTeammateHarness
	detectTeammateHarness = func() (harness.Detection, harness.Outcome) { return d, outcome }
	t.Cleanup(func() { detectTeammateHarness = previous })
}

// Spec property 1: explicit presence wins, including empty, across every mode
// and detection outcome. Values vary independently from the source states.
func TestPropertyTeammatePrecedence(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	original := detectTeammateHarness
	t.Cleanup(func() { detectTeammateHarness = original })
	hegel.Test(t, func(ht *hegel.T) {
		flagState := hegel.Draw(ht, hegel.SampledFrom([]int{0, 1, 2}))
		envState := hegel.Draw(ht, hegel.SampledFrom([]int{0, 1, 2}))
		mode := hegel.Draw(ht, hegel.SampledFrom([]string{"model", "harness", "off"}))
		outcome := hegel.Draw(ht, hegel.SampledFrom([]harness.Outcome{harness.None, harness.Detected, harness.Ambiguous}))
		d := harness.Detection{Harness: hegel.Draw(ht, hegel.SampledFrom(harness.Names())), Model: hegel.Draw(ht, hegel.Text()), Provider: hegel.Draw(ht, hegel.Text())}
		flagValue := fmt.Sprintf("flag-%x", hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
		envValue := fmt.Sprintf("env-%x", hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
		if flagState == 1 {
			flagValue = ""
		}
		if envState == 1 {
			envValue = ""
		}
		cmd := teammateTestCommand()
		if flagState != 0 {
			require.NoError(t, cmd.Flags().Set("teammate", flagValue))
		}
		if envState == 0 {
			_ = os.Unsetenv("KATA_TEAMMATE")
		} else {
			t.Setenv("KATA_TEAMMATE", envValue)
		}
		t.Setenv("KATA_TEAMMATE_INFER", mode)
		detectTeammateHarness = func() (harness.Detection, harness.Outcome) { return d, outcome }
		got, err := resolveTeammate(cmd)
		want := teammateResolution{}
		switch {
		case flagState != 0:
			want.Handle, want.Source = flagValue, "flag"
		case envState != 0:
			want.Handle, want.Source = envValue, "env"
		case mode == "off":
			want.Source = "disabled"
		case outcome == harness.Detected:
			want.Handle, want.Source, want.Harness = harness.Handle(d, harness.Mode(mode)), "inferred", d.Harness
		case outcome == harness.Ambiguous:
			want.Source = "ambiguous"
		default:
			want.Source = "none"
		}
		if err != nil || got != want {
			ht.Fatalf("flag=%d env=%d mode=%s outcome=%d: got %+v/%v want %+v", flagState, envState, mode, outcome, got, err, want)
		}
	}, hegel.WithTestCases(500), hegel.WithDatabase(t.TempDir()))
}

// Spec property 14: changing inference never changes the explicit identity
// consumed by notify/wait; inbox's explicit recipient is independent too.
func TestPropertyAddressingIgnoresInference(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	fakeTeammateDetection(t, harness.Detection{Harness: "codex", Model: "model"}, harness.Detected)
	hegel.Test(t, func(ht *hegel.T) {
		handle := fmt.Sprintf("worker-%x", hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
		state := hegel.Draw(ht, hegel.SampledFrom([]int{0, 1, 2, 3, 4}))
		cmd := teammateTestCommand()
		_ = os.Unsetenv("KATA_TEAMMATE")
		want := ""
		switch state {
		case 1:
			t.Setenv("KATA_TEAMMATE", handle)
			want = handle
		case 2:
			_ = cmd.Flags().Set("teammate", handle)
			want = handle
		case 3:
			t.Setenv("KATA_TEAMMATE", handle)
			_ = cmd.Flags().Set("teammate", "")
		case 4:
			t.Setenv("KATA_TEAMMATE", "")
		}
		for _, mode := range []string{"model", "off"} {
			t.Setenv("KATA_TEAMMATE_INFER", mode)
			got, err := explicitTeammate(cmd)
			if err != nil || got != want {
				ht.Fatalf("state=%d mode=%s got %q/%v want %q", state, mode, got, err, want)
			}
		}
	}, hegel.WithTestCases(200), hegel.WithDatabase(t.TempDir()))
}

func TestResolveTeammateInvalidExplicitNeverFallsBack(t *testing.T) {
	t.Setenv("KATA_TEAMMATE", "@bad")
	fakeTeammateDetection(t, harness.Detection{Harness: "codex"}, harness.Detected)
	_, err := resolveTeammate(teammateTestCommand())
	require.ErrorContains(t, err, "teammate must be")
}

func TestTeammateInferEnvOverridesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[teammate]\ninfer='harness'"), 0600))
	fakeTeammateDetection(t, harness.Detection{Harness: "pi", Model: "example"}, harness.Detected)
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	got, err := resolveTeammate(teammateTestCommand())
	require.NoError(t, err)
	require.Equal(t, "pi.example", got.Handle)
	for _, invalid := range []string{"", "maybe", "MODEL"} {
		t.Setenv("KATA_TEAMMATE_INFER", invalid)
		_, err = resolveTeammate(teammateTestCommand())
		require.ErrorContains(t, err, "KATA_TEAMMATE_INFER")
	}
}

func TestResolveTeammateCachedOnce(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	previous := detectTeammateHarness
	t.Cleanup(func() { detectTeammateHarness = previous })
	calls := 0
	detectTeammateHarness = func() (harness.Detection, harness.Outcome) {
		calls++
		return harness.Detection{Harness: "pi"}, harness.Detected
	}
	cmd := teammateTestCommand()
	first, err := resolveTeammate(cmd)
	require.NoError(t, err)
	t.Setenv("KATA_TEAMMATE", "changed")
	second, err := resolveTeammate(cmd)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 1, calls)
}

func TestResolveTeammateCachesErrorsAndAddressingBoundary(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid_explicit=%t", explicit), func(t *testing.T) {
			t.Setenv("KATA_HOME", t.TempDir())
			unsetTeammateEnv(t, "KATA_TEAMMATE")
			t.Setenv("KATA_TEAMMATE_INFER", "invalid")
			if explicit {
				t.Setenv("KATA_TEAMMATE", "bad/handle")
			}
			cmd := teammateTestCommand()
			first, firstErr := resolveTeammate(cmd)
			require.Error(t, firstErr)
			t.Setenv("KATA_TEAMMATE", "repaired")
			t.Setenv("KATA_TEAMMATE_INFER", "off")
			second, secondErr := resolveTeammate(cmd)
			require.Equal(t, first, second)
			require.Same(t, firstErr, secondErr)
			address, err := explicitTeammate(cmd)
			if explicit {
				require.Equal(t, "bad/handle", address)
				require.Same(t, firstErr, err)
			} else {
				require.NoError(t, err, "inference errors must not block explicit-only addressing")
				require.Empty(t, address)
			}
			fresh, err := resolveTeammate(teammateTestCommand())
			require.NoError(t, err)
			require.Equal(t, teammateResolution{Handle: "repaired", Source: "env"}, fresh)
		})
	}
}

func TestResolveTeammateSkipsLowerPriorityInputs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("[teammate\ninfer ="), 0o600))
	previous := detectTeammateHarness
	t.Cleanup(func() { detectTeammateHarness = previous })
	detectTeammateHarness = func() (harness.Detection, harness.Outcome) {
		t.Fatal("explicit selection and off must never consult the detector")
		return harness.Detection{}, harness.None
	}
	for _, tc := range []struct {
		name, source, handle string
	}{
		{"flag", "flag", "worker"}, {"empty flag", "flag", ""},
		{"environment", "env", "worker"}, {"empty environment", "env", ""}, {"off", "disabled", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unsetTeammateEnv(t, "KATA_TEAMMATE")
			t.Setenv("KATA_TEAMMATE_INFER", "invalid")
			cmd := teammateTestCommand()
			switch tc.source {
			case "flag":
				t.Setenv("KATA_TEAMMATE", "bad/handle")
				require.NoError(t, cmd.Flags().Set("teammate", tc.handle))
			case "env":
				t.Setenv("KATA_TEAMMATE", tc.handle)
			default:
				t.Setenv("KATA_TEAMMATE_INFER", "off")
			}
			got, err := resolveTeammate(cmd)
			require.NoError(t, err)
			require.Equal(t, teammateResolution{Handle: tc.handle, Source: tc.source}, got)
		})
	}
}

// Hegel shrank the direct-command nil-context panic to absent explicit values,
// model mode and no detected harness (the drawn candidate was amp).
func TestResolveTeammateDirectCommandNoHarness(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	fakeTeammateDetection(t, harness.Detection{Harness: "amp"}, harness.None)
	got, err := resolveTeammate(teammateTestCommand())
	require.NoError(t, err)
	require.Equal(t, teammateResolution{Source: "none"}, got)
}

func TestTeammateMCPTransportStartupDefaults(t *testing.T) {
	t.Setenv("KATA_HOME", t.TempDir())
	unsetTeammateEnv(t, "KATA_TEAMMATE")
	t.Setenv("KATA_TEAMMATE_INFER", "model")
	previous := detectTeammateHarness
	t.Cleanup(func() { detectTeammateHarness = previous })
	calls := 0
	detectTeammateHarness = func() (harness.Detection, harness.Outcome) {
		calls++
		return harness.Detection{Harness: "pi"}, harness.Detected
	}
	for _, tc := range []struct {
		http, explicit, want, mode string
		detectionCount             int
	}{
		{"", "", "pi", "model", 1}, {"127.0.0.1:0", "", "", "model", 1}, {"127.0.0.1:0", "explicit-worker", "explicit-worker", "model", 1},
		{"127.0.0.1:0", "", "", "invalid", 1},
	} {
		t.Setenv("KATA_TEAMMATE_INFER", tc.mode)
		parent := newMCPCmd()
		cmd, _, err := parent.Find([]string{"serve"})
		require.NoError(t, err)
		cmd.Flags().String("teammate", "", "")
		require.NoError(t, cmd.Flags().Set("http", tc.http))
		if tc.explicit != "" {
			require.NoError(t, cmd.Flags().Set("teammate", tc.explicit))
		}
		got, err := resolveTeammate(cmd)
		require.NoError(t, err)
		require.Equal(t, tc.want, got.Handle)
		require.Equal(t, tc.detectionCount, calls)
	}
}

func TestTeammateExplicitAddressingUsesCachedSelection(t *testing.T) {
	t.Setenv("KATA_TEAMMATE", "first-worker")
	cmd := teammateTestCommand()
	_, err := resolveTeammate(cmd)
	require.NoError(t, err)
	t.Setenv("KATA_TEAMMATE", "second-worker")
	got, err := explicitTeammate(cmd)
	require.NoError(t, err)
	require.Equal(t, "first-worker", got)
}
