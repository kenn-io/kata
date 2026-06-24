package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/selfupdate"
)

type fakeUpdateClient struct {
	checks       []selfupdate.CheckOptions
	checkResults []*selfupdate.Info
	checkErr     error
	installed    []*selfupdate.Info
	installErr   error
}

func (f *fakeUpdateClient) Check(_ context.Context, opts selfupdate.CheckOptions) (*selfupdate.Info, error) {
	f.checks = append(f.checks, opts)
	if f.checkErr != nil {
		return nil, f.checkErr
	}
	if len(f.checkResults) == 0 {
		return nil, nil
	}
	info := f.checkResults[0]
	f.checkResults = f.checkResults[1:]
	return info, nil
}

func (f *fakeUpdateClient) Install(_ context.Context, info *selfupdate.Info, _ selfupdate.InstallOptions) error {
	f.installed = append(f.installed, info)
	return f.installErr
}

func stubUpdateClient(t *testing.T, client updateClient) {
	t.Helper()
	orig := newSelfUpdateClient
	newSelfUpdateClient = func(string) (updateClient, error) {
		return client, nil
	}
	t.Cleanup(func() { newSelfUpdateClient = orig })
}

func TestUpdate_IsWiredOnRoot(t *testing.T) {
	resetFlags(t)
	root := newRootCmd()
	_, _, err := root.Find([]string{"update"})
	require.NoError(t, err)
}

func TestUpdateCheck_HumanUpToDate(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.5.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{}
	stubUpdateClient(t, fake)

	stdout, _, err := executeRootCapture(t, context.Background(), "update", "--check")

	require.NoError(t, err)
	assert.Equal(t, "kata is up to date (v0.5.0)\n", stdout)
	assert.Len(t, fake.checks, 1)
	assert.False(t, fake.checks[0].Force)
	assert.Empty(t, fake.installed)
}

func TestUpdateCheck_HumanUpdateAvailable(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.4.0",
		LatestVersion:  "v0.5.0",
		AssetName:      "kata_0.5.0_linux_amd64.tar.gz",
	}}}
	stubUpdateClient(t, fake)

	stdout, _, err := executeRootCapture(t, context.Background(), "update", "--check")

	require.NoError(t, err)
	assert.Equal(t, "update available: v0.4.0 -> v0.5.0\n", stdout)
	assert.Empty(t, fake.installed)
}

func TestUpdateCheck_DevBuildForcesFreshCheckAndShowsOfficialRelease(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.5.0-9-gcf994f5", "cf994f5", "2026-06-24T15:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.5.0-9-gcf994f5",
		LatestVersion:  "v0.6.0",
		AssetName:      "kata_0.6.0_linux_amd64.tar.gz",
		IsDevBuild:     true,
	}}}
	stubUpdateClient(t, fake)

	stdout, _, err := executeRootCapture(t, context.Background(), "update", "--check")

	require.NoError(t, err)
	assert.Equal(t, "dev build: v0.5.0-9-gcf994f5\nlatest official release: v0.6.0\nUse 'kata update --force' to install the latest official release.\n", stdout)
	require.Len(t, fake.checks, 1)
	assert.True(t, fake.checks[0].Force)
	assert.Empty(t, fake.installed)
}

func TestUpdateCheck_JSONOutput(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.4.0",
		LatestVersion:  "v0.5.0",
		AssetName:      "kata_0.5.0_linux_amd64.tar.gz",
		IsDevBuild:     true,
	}}}
	stubUpdateClient(t, fake)

	stdout, _, err := executeRootCapture(t, context.Background(), "--json", "update", "--check")

	require.NoError(t, err)
	var got struct {
		KataAPIVersion  int    `json:"kata_api_version"`
		CurrentVersion  string `json:"current_version"`
		LatestVersion   string `json:"latest_version"`
		UpdateAvailable bool   `json:"update_available"`
		AssetName       string `json:"asset_name"`
		IsDevBuild      bool   `json:"is_dev_build"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "v0.4.0", got.CurrentVersion)
	assert.Equal(t, "v0.5.0", got.LatestVersion)
	assert.True(t, got.UpdateAvailable)
	assert.Equal(t, "kata_0.5.0_linux_amd64.tar.gz", got.AssetName)
	assert.True(t, got.IsDevBuild)
}

func TestUpdateCheck_AgentOutput(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.4.0",
		LatestVersion:  "v0.5.0",
	}}}
	stubUpdateClient(t, fake)

	stdout, _, err := executeRootCapture(t, context.Background(), "--agent", "update", "--check")

	require.NoError(t, err)
	assert.Equal(t, "OK update update_available=true current=v0.4.0 latest=v0.5.0\n", stdout)
}

func TestUpdateInstall_HumanShowsDownloadDetailsBeforeConfirmation(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.4.0",
		LatestVersion:  "v0.5.0",
		DownloadURL:    "https://example.test/kata_0.5.0_linux_amd64.tar.gz",
		AssetName:      "kata_0.5.0_linux_amd64.tar.gz",
		Size:           2048,
		Checksum:       "abc123checksum",
	}}}
	stubUpdateClient(t, fake)

	stdout, stderr, err := executeRootCaptureWithInput(context.Background(), t, "n\n", "update")

	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Equal(t, "\nCurrent version: v0.4.0\nLatest version:  v0.5.0\n\nUpdate available.\n\nDownload:\n  URL:    https://example.test/kata_0.5.0_linux_amd64.tar.gz\n  Size:   2.0 KB\n  SHA256: abc123checksum\n\n", stdout)
	assert.Contains(t, stderr, "Install kata update v0.4.0 -> v0.5.0?")
	assert.Empty(t, fake.installed)
}

func TestUpdateInstall_DevBuildRequiresForce(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.5.0-9-gcf994f5", "cf994f5", "2026-06-24T15:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.5.0-9-gcf994f5",
		LatestVersion:  "v0.6.0",
		DownloadURL:    "https://example.test/kata_0.6.0_linux_amd64.tar.gz",
		AssetName:      "kata_0.6.0_linux_amd64.tar.gz",
		Size:           4096,
		Checksum:       "def456checksum",
		IsDevBuild:     true,
	}}}
	stubUpdateClient(t, fake)

	stdout, stderr, err := executeRootCapture(t, context.Background(), "update")

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "\nCurrent version: v0.5.0-9-gcf994f5\nLatest version:  v0.6.0\n\nYou're running a dev build. Latest official release available.\n\nDownload:\n  URL:    https://example.test/kata_0.6.0_linux_amd64.tar.gz\n  Size:   4.0 KB\n  SHA256: def456checksum\n\nUse 'kata update --force' to install the latest official release.\n", stdout)
	require.Len(t, fake.checks, 1)
	assert.True(t, fake.checks[0].Force)
	assert.Empty(t, fake.installed)
}

func TestUpdateInstall_DevBuildForceInstalls(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.5.0-9-gcf994f5", "cf994f5", "2026-06-24T15:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.5.0-9-gcf994f5",
		LatestVersion:  "v0.6.0",
		AssetName:      "kata_0.6.0_linux_amd64.tar.gz",
		IsDevBuild:     true,
	}}}
	stubUpdateClient(t, fake)

	stdout, _, err := executeRootCapture(t, context.Background(), "update", "--force", "--yes")

	require.NoError(t, err)
	assert.Equal(t, "installed kata v0.6.0\n", stdout)
	assert.Len(t, fake.installed, 1)
}

func TestUpdateInstall_RefetchesCachedInfoBeforeInstall(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	first := &selfupdate.Info{CurrentVersion: "v0.4.0", LatestVersion: "v0.5.0", AssetName: "kata_0.5.0_linux_amd64.tar.gz"}
	second := &selfupdate.Info{CurrentVersion: "v0.4.0", LatestVersion: "v0.5.0", AssetName: "kata_0.5.0_linux_amd64.tar.gz", DownloadURL: "https://example.test/kata"}
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{first, second}}
	stubUpdateClient(t, fake)

	origNeedsRefetch := updateInfoNeedsRefetch
	updateInfoNeedsRefetch = func(info *selfupdate.Info) bool { return info == first }
	t.Cleanup(func() { updateInfoNeedsRefetch = origNeedsRefetch })

	stdout, _, err := executeRootCapture(t, context.Background(), "update", "--yes")

	require.NoError(t, err)
	assert.Equal(t, "installed kata v0.5.0\n", stdout)
	require.Len(t, fake.checks, 2)
	assert.False(t, fake.checks[0].Force)
	assert.True(t, fake.checks[1].Force)
	assert.Equal(t, []*selfupdate.Info{second}, fake.installed)
}

func TestUpdateInstall_JSONRequiresConfirmation(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.4.0",
		LatestVersion:  "v0.5.0",
		AssetName:      "kata_0.5.0_linux_amd64.tar.gz",
	}}}
	stubUpdateClient(t, fake)

	stdout, stderr, err := executeRootCaptureWithInput(context.Background(), t, "n\n", "--json", "update")

	ce := requireCLIError(t, err, ExitConfirm)
	assert.Equal(t, kindConfirm, ce.Kind)
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Install kata update v0.4.0 -> v0.5.0?")
	assert.Empty(t, fake.installed)
}

func TestUpdateInstall_JSONOutputAfterConfirmation(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.4.0",
		LatestVersion:  "v0.5.0",
		AssetName:      "kata_0.5.0_linux_amd64.tar.gz",
	}}}
	stubUpdateClient(t, fake)

	stdout, stderr, err := executeRootCaptureWithInput(context.Background(), t, "y\n", "--json", "update")

	require.NoError(t, err)
	assert.Contains(t, stderr, "Install kata update v0.4.0 -> v0.5.0?")
	var got struct {
		KataAPIVersion  int    `json:"kata_api_version"`
		CurrentVersion  string `json:"current_version"`
		LatestVersion   string `json:"latest_version"`
		UpdateAvailable bool   `json:"update_available"`
		Installed       bool   `json:"installed"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &got))
	assert.Equal(t, 1, got.KataAPIVersion)
	assert.Equal(t, "v0.4.0", got.CurrentVersion)
	assert.Equal(t, "v0.5.0", got.LatestVersion)
	assert.True(t, got.UpdateAvailable)
	assert.True(t, got.Installed)
	assert.Len(t, fake.installed, 1)
}

func TestUpdateInstall_AgentOutputAfterYes(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{checkResults: []*selfupdate.Info{{
		CurrentVersion: "v0.4.0",
		LatestVersion:  "v0.5.0",
	}}}
	stubUpdateClient(t, fake)

	stdout, stderr, err := executeRootCapture(t, context.Background(), "--agent", "update", "--yes")

	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "OK update installed=true current=v0.4.0 latest=v0.5.0\n", stdout)
}

func TestUpdateInstall_WrapsInstallError(t *testing.T) {
	resetFlags(t)
	stubVersionInfo(t, "v0.4.0", "abc1234", "2026-06-19T12:00:00Z")
	fake := &fakeUpdateClient{
		checkResults: []*selfupdate.Info{{
			CurrentVersion: "v0.4.0",
			LatestVersion:  "v0.5.0",
			AssetName:      "kata_0.5.0_linux_amd64.tar.gz",
		}},
		installErr: errors.New("permission denied"),
	}
	stubUpdateClient(t, fake)

	_, _, err := executeRootCapture(t, context.Background(), "update", "--yes")

	ce := requireCLIError(t, err, ExitInternal)
	assert.Equal(t, kindInternal, ce.Kind)
	assert.True(t, strings.HasPrefix(ce.Message, "install update: "))
	assert.Contains(t, ce.Message, "permission denied")
}

func TestUpdate_DefaultClientConfiguration(t *testing.T) {
	resetFlags(t)
	home := t.TempDir()
	t.Setenv("KATA_HOME", home)

	client, err := newSelfUpdateClient("v0.4.0")

	require.NoError(t, err)
	var got selfupdate.Client
	switch c := client.(type) {
	case selfupdate.Client:
		got = c
	case *selfupdate.Client:
		got = *c
	default:
		t.Fatalf("newSelfUpdateClient returned %T, want selfupdate.Client", client)
	}
	assert.Equal(t, "kenn-io", got.Owner)
	assert.Equal(t, "kata", got.Repo)
	assert.Equal(t, "kata", got.BinaryName)
	assert.Equal(t, "v0.4.0", got.CurrentVersion)
	assert.Equal(t, filepath.Join(home, "cache", "update"), got.CacheDir)
	assert.True(t, got.AllowUnsignedChecksums)
	assert.False(t, got.RequireSignature)
	assert.Empty(t, got.TrustedPublicKeys)
}

func executeRootCaptureWithInput(ctx context.Context, t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	resetFlags(t)
	cmd := newRootCmd()
	var so, se bytes.Buffer
	cmd.SetOut(&so)
	cmd.SetErr(&se)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	err = cmd.Execute()
	if err != nil {
		emitRootError(&se, cmd, args, err, runEEntered)
	}
	return so.String(), se.String(), err
}
