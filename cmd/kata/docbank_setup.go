package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/docbank"
)

// localDocbank finds the Docbank on this machine; tests replace it to stub starting Docbank.
// The home is always DOCBANK_HOME or ~/.docbank.
var localDocbank = docbank.Local{}

// docbankSection is the [docbank] table setup appends; credentials stay in the key file.
type docbankSection struct {
	Local      bool   `toml:"local,omitempty"`
	URL        string `toml:"url,omitempty"`
	APIKeyFile string `toml:"api_key_file,omitempty"`
	Folder     string `toml:"folder"`
}

func newDocbankSetupCmd() *cobra.Command {
	var docbankURL, keyFile, folder string
	var yes bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "point the daemon at a Docbank for attachments",
		Long: "Without --url, use the Docbank on this machine, starting it when needed and following its port and key " +
			"changes. With --url and --key-file, use a Docbank that already runs elsewhere. Both forms check the key, " +
			"add [docbank] to <KATA_HOME>/config.toml and restart running daemons.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := requireCurrentHomeDaemonCommand(); err != nil {
				return err
			}
			for _, key := range []string{"KATA_DOCBANK_URL", "KATA_DOCBANK_API_KEY_FILE", "KATA_DOCBANK_FOLDER"} {
				if strings.TrimSpace(os.Getenv(key)) != "" {
					return fmt.Errorf("unset %s, then rerun; it would override the [docbank] section setup writes", key)
				}
			}
			local := docbankURL == ""
			switch {
			case local && keyFile != "":
				return &cliError{Message: "--key-file goes with --url; the Docbank on this machine supplies its own key", Kind: kindValidation, ExitCode: ExitValidation}
			case !local && keyFile == "":
				return &cliError{Message: "--url needs --key-file naming a private file that holds the Docbank API key", Kind: kindValidation, ExitCode: ExitValidation}
			}
			path, err := config.DaemonConfigPath()
			if err != nil {
				return err
			}
			if !local && keyFile != "~" && !strings.HasPrefix(keyFile, "~/") {
				if keyFile, err = filepath.Abs(keyFile); err != nil {
					return err
				}
			}
			want, err := config.NormalizeDocbankConfig(config.DocbankConfig{Local: local, URL: docbankURL, APIKeyFile: keyFile, Folder: folder})
			if err != nil {
				return &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
			}
			original, originalExists, err := readDaemonConfig(path)
			if err != nil {
				return err
			}
			have, err := config.ParseDocbankFile(original)
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			hasSection := have != config.DocbankConfig{}
			if hasSection {
				if have, err = config.NormalizeDocbankConfig(have); err != nil || have != want {
					return fmt.Errorf("%s already has a different [docbank] section; edit or remove it, then rerun", path)
				}
			}
			vault, label := want.URL, want.URL
			var key string
			if local {
				if vault, err = docbank.LocalHome(); err != nil {
					return err
				}
				label = "the Docbank on this machine (" + vault + ")"
			} else if key, err = want.ResolveKey(); err != nil {
				return err
			}
			changed := !hasSection
			if changed {
				warning := "Every issue reader and metadata writer served by this daemon can read any file in the Docbank vault at " + vault
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), warning); err != nil {
					return err
				}
				if !yes {
					if err := resolveYesNo(cmd, "Configure Docbank attachments and restart running daemons? [y/N] "); err != nil {
						if cli, ok := errors.AsType[*cliError](err); ok && cli.Code == "confirm_mismatch" {
							cli.Message = "docbank setup cancelled"
						}
						return err
					}
				}
			}
			// Starting Docbank and contacting a remote one wait for approval, but still precede any write.
			var client *docbank.Client
			if local {
				if err = localDocbank.Ready(ctx); err != nil {
					return fmt.Errorf("cannot use the Docbank on this machine: %w; kata's config was not changed", err)
				}
				client = docbank.NewLocalClient(localDocbank)
			} else {
				client = docbank.NewClient(want.URL, func() (string, error) { return key, nil })
			}
			if err := checkDocbankKey(ctx, client, label); err != nil {
				return err
			}
			if changed {
				proposed, err := appendTOMLTable(original, "docbank", docbankSection{Local: want.Local, URL: want.URL, APIKeyFile: want.APIKeyFile, Folder: want.Folder})
				if err != nil {
					return fmt.Errorf("cannot add [docbank] to %s: %w", path, err)
				}
				if written, err := config.ParseDocbankFile(proposed); err != nil || written != want {
					return fmt.Errorf("cannot add [docbank] to %s: the file already sets docbank keys", path)
				}
				if err := writeDaemonConfigAndRestart(ctx, cmd.ErrOrStderr(), path, original, originalExists, proposed); err != nil {
					return err
				}
			}
			return printDocbankSetup(cmd.OutOrStdout(), want, label, changed)
		},
	}
	cmd.Flags().StringVar(&docbankURL, "url", "", "use a Docbank running elsewhere; HTTPS except on loopback")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "private file holding that Docbank's API key (with --url)")
	cmd.Flags().StringVar(&folder, "folder", "", "Docbank folder for uploads (default /kata)")
	cmd.Flags().BoolVar(&yes, "yes", false, "approve the configuration change and restarts")
	return cmd
}

// printDocbankSetup reports the result; target names the Docbank (the machine's Docbank for the local form, the URL otherwise).
func printDocbankSetup(w io.Writer, want config.DocbankConfig, target string, changed bool) error {
	var err error
	switch currentOutputMode() {
	case outputJSON:
		return emitJSON(w, struct {
			Local   bool   `json:"local"`
			URL     string `json:"url,omitempty"`
			Folder  string `json:"folder"`
			KeyFile string `json:"key_file,omitempty"`
			Changed bool   `json:"changed"`
		}{want.Local, want.URL, want.Folder, want.APIKeyFile, changed})
	case outputAgent:
		if want.Local {
			_, err = fmt.Fprintf(w, "OK docbank-setup local=true folder=%s changed=%t\n", agentValue(want.Folder), changed)
		} else {
			_, err = fmt.Fprintf(w, "OK docbank-setup url=%s folder=%s changed=%t\n", agentValue(want.URL), agentValue(want.Folder), changed)
		}
	case outputHuman:
		if changed {
			_, err = fmt.Fprintf(w, "Docbank attachments use %s, folder %s.\n", target, want.Folder)
		} else {
			_, err = fmt.Fprintf(w, "Docbank attachments are already set up: %s, folder %s.\n", target, want.Folder)
		}
	}
	return err
}

// checkDocbankKey reads Docbank's root folder through client, which carries the key the daemon will use.
// label names the Docbank in errors.
func checkDocbankKey(ctx context.Context, client *docbank.Client, label string) error {
	_, err := client.ResolvePath(ctx, "/")
	status, ok := errors.AsType[*docbank.Error](err)
	switch {
	case err == nil:
		return nil
	case ok && (status.Status == 401 || status.Status == 403):
		return fmt.Errorf("%s rejected the API key (HTTP %d); kata's config was not changed", label, status.Status)
	case ok && status.Status != 0:
		return fmt.Errorf("%s answered HTTP %d to a read of its root folder; kata's config was not changed", label, status.Status)
	default:
		return fmt.Errorf("cannot reach %s; kata's config was not changed", label)
	}
}

// appendTOMLTable keeps every existing byte and adds the table after them.
func appendTOMLTable(original []byte, name string, table any) ([]byte, error) {
	var encoded bytes.Buffer
	enc := toml.NewEncoder(&encoded)
	enc.Indent = ""
	if err := enc.Encode(map[string]any{name: table}); err != nil {
		return nil, err
	}
	proposed := bytes.Clone(original)
	if len(proposed) > 0 {
		if proposed[len(proposed)-1] != '\n' {
			proposed = append(proposed, '\n')
		}
		proposed = append(proposed, '\n')
	}
	return append(proposed, encoded.Bytes()...), nil
}
