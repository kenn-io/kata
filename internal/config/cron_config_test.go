package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadDaemonCronExecution(t *testing.T) {
	for _, tc := range []struct {
		name, document string
		wantErr        string
		want           CronConfig
	}{
		{name: "default"},
		{name: "mode trim, exact project name", want: CronConfig{ReplicaExecute: "owned", Projects: []CronProjectConfig{{Name: " spoke-project ", Execute: "all"}}}, document: "[cron]\nreplica_execute = ' owned '\n[[cron.project]]\nname = ' spoke-project '\nexecute = ' all '\n"},
		{name: "none", document: "[cron]\nreplica_execute = 'none'", want: CronConfig{ReplicaExecute: "none"}},
		{name: "all", document: "[cron]\nreplica_execute = 'all'", want: CronConfig{ReplicaExecute: "all"}},
		{name: "duplicate names", document: "[[cron.project]]\nname = 'spoke-project'\nexecute = 'all'\n[[cron.project]]\nname = 'spoke-project'\nexecute = 'none'", wantErr: "duplicate name"},
		{name: "bad default", document: "[cron]\nreplica_execute = 'yes'", wantErr: "cron.replica_execute"},
		{name: "bad override", document: "[[cron.project]]\nname = 'spoke-project'\nexecute = 'yes'", wantErr: "cron.project[0].execute"},
		{name: "missing mode", document: "[[cron.project]]\nname = 'spoke-project'", wantErr: "cron.project[0].execute"},
		{name: "bad name", document: "[[cron.project]]\nname = ' '\nexecute = 'all'", wantErr: "cron.project[0].name"},
		{name: "distinct exact names", want: CronConfig{Projects: []CronProjectConfig{{Name: "spoke-project", Execute: "all"}, {Name: " spoke-project ", Execute: "none"}}}, document: "[[cron.project]]\nname = 'spoke-project'\nexecute = 'all'\n[[cron.project]]\nname = ' spoke-project '\nexecute = 'none'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("KATA_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(tc.document), 0600))
			cfg, err := ReadDaemonConfig()
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Cron)
		})
	}
}
