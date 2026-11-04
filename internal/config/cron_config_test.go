package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestReadDaemonCronExecution(t *testing.T) {
	for _, tc := range []struct {
		name, document string
		wantErr        bool
	}{
		{name: "default"},
		{name: "mode trim, exact project name", document: "[cron]\nreplica_execute = ' owned '\n[[cron.project]]\nname = ' company-project '\nexecute = ' all '\n"},
		{name: "bad default", document: "[cron]\nreplica_execute = 'yes'", wantErr: true},
		{name: "bad override", document: "[[cron.project]]\nname = 'company-project'\nexecute = 'yes'", wantErr: true},
		{name: "missing mode", document: "[[cron.project]]\nname = 'company-project'", wantErr: true},
		{name: "bad name", document: "[[cron.project]]\nname = ' '\nexecute = 'all'", wantErr: true},
		{name: "unknown key", document: "[cron]\nexecute = 'all'", wantErr: true},
		{name: "distinct exact names", document: "[[cron.project]]\nname = 'company-project'\nexecute = 'all'\n[[cron.project]]\nname = ' company-project '\nexecute = 'none'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.document), 0600))
			cfg, err := ReadDaemonConfig()
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.name == "mode trim, exact project name" {
				require.Equal(t, "owned", cfg.Cron.ReplicaExecute)
				require.Equal(t, []CronProjectConfig{{Name: " company-project ", Execute: "all"}}, cfg.Cron.Projects)
			}
			if tc.name == "distinct exact names" {
				require.Equal(t, []CronProjectConfig{{Name: "company-project", Execute: "all"}, {Name: " company-project ", Execute: "none"}}, cfg.Cron.Projects)
			}
		})
	}
}

func TestCronConfigModes(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mode := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"none", "owned", "all"})))
		valid := mode == "none" || mode == "owned" || mode == "all"
		require.Equal(ht, !valid, (CronConfig{Projects: []CronProjectConfig{{Name: "company-project", Execute: mode}}}).Validate() != nil, strconv.Quote(mode))
	}, hegel.WithTestCases(1000))
}

func TestCronConfigDuplicateNames(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		suffix := hegel.Draw(ht, hegel.Integers(0, 1000000))
		name := "project-" + strconv.Itoa(suffix)
		first := hegel.Draw(ht, hegel.SampledFrom([]string{"none", "owned", "all"}))
		second := hegel.Draw(ht, hegel.SampledFrom([]string{"none", "owned", "all"}))
		require.Error(ht, (CronConfig{Projects: []CronProjectConfig{{Name: name, Execute: first}, {Name: name, Execute: second}}}).Validate())
	}, hegel.WithTestCases(1000))
}

func TestCronConfigDefaultModes(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mode := hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"", "none", "owned", "all"})))
		require.Equal(ht, mode != "" && mode != "none" && mode != "owned" && mode != "all", (CronConfig{ReplicaExecute: mode}).Validate() != nil)
	}, hegel.WithTestCases(1000))
}

func TestCronConfigProjectNames(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		require.Equal(ht, ValidateProjectName(name) != nil, (CronConfig{Projects: []CronProjectConfig{{Name: name, Execute: "all"}}}).Validate() != nil)
	}, hegel.WithTestCases(1000))
}

func TestCronConfigDistinctNamesWithWhitespace(t *testing.T) {
	cfg := CronConfig{Projects: []CronProjectConfig{{Name: " spoke-project ", Execute: "owned"}, {Name: "spoke-project", Execute: "none"}}}
	trimCronConfig(&cfg)
	require.NoError(t, cfg.Validate())
}
