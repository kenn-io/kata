package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/httpurl"
)

func signingSource(base, id, file, env string) (*federationsigning.Source, error) {
	if id == "" && file == "" && env == "" {
		return nil, nil
	}
	canonical, err := httpurl.CanonicalHTTPBaseURL(base)
	if err != nil {
		return nil, err
	}
	if file != "" {
		file, err = filepath.Abs(file)
		if err != nil {
			return nil, fmt.Errorf("resolve federation signing key file: %w", err)
		}
	}
	source := federationsigning.Source{KeyID: id, KeyFile: file, KeyEnv: env, HubURL: canonical}
	if err := federationsigning.ValidatePolicy(source.HubURL, []federationsigning.Key{{Source: source, EnrollmentID: 1}}); err != nil {
		return nil, err
	}
	return &source, nil
}

func federationSigningCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "signing", Short: "configure local federation signing references"}
	var uid, id, file, env, hub string
	configure := &cobra.Command{Use: "configure", Args: cobra.NoArgs, Short: "select a signing source for an existing enrollment", RunE: func(cmd *cobra.Command, _ []string) error {
		store := config.DefaultFederationCredentialStore()
		current, found, err := store.FederationCredential(cmd.Context(), uid)
		if err != nil {
			return err
		}
		if !found || current.LeavePending {
			return fmt.Errorf("active federation credential not found")
		}
		if hub == "" {
			hub = current.HubURL
		}
		source, err := signingSource(hub, id, file, env)
		if err != nil {
			return err
		}
		if source == nil {
			return fmt.Errorf("signing key references are required")
		}
		replacement := current
		replacement.Signing = source
		replacer, ok := store.(config.FederationCredentialReplacer)
		if !ok {
			return fmt.Errorf("credential store does not support exact replacement")
		}
		return replacer.ReplaceFederationCredential(cmd.Context(), config.FederationCredentialReplacement{ProjectUID: uid, Expected: current, Replacement: replacement})
	}}
	configure.Flags().StringVar(&uid, "project-uid", "", "local project UID")
	configure.Flags().StringVar(&id, "key-id", "", "public signing key ID")
	configure.Flags().StringVar(&file, "key-file", "", "owner-only file containing the independent signing secret")
	configure.Flags().StringVar(&env, "key-env", "", "environment variable containing the independent signing secret")
	configure.Flags().StringVar(&hub, "hub-url", "", "explicit HTTPS base for this signing key, including any prefix")
	_ = configure.MarkFlagRequired("project-uid")
	var state string
	initialize := &cobra.Command{Use: "init-replay", Args: cobra.NoArgs, Short: "create new replay state without replacing existing state", RunE: func(_ *cobra.Command, _ []string) error {
		if state == "" {
			home, err := config.KataHome()
			if err != nil {
				return err
			}
			state = filepath.Join(home, "federation-signing-replay.state")
		} else if !filepath.IsAbs(state) {
			return fmt.Errorf("state-file must be an absolute path")
		}
		return federationsigning.InitializeReplayState(state)
	}}
	initialize.Flags().StringVar(&state, "state-file", "", "private replay state file")
	cmd.AddCommand(configure, initialize)
	return cmd
}
