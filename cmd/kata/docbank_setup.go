package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/docbank"
	"go.kenn.io/kit/atomicfile"
)

// defaultDocbankPort is the fixed port a local Docbank gets when neither --port nor its config names one.
const defaultDocbankPort = 8080

// runDocbank runs the Docbank CLI and returns its standard output.
var runDocbank = func(ctx context.Context, args ...string) ([]byte, error) {
	if _, err := exec.LookPath("docbank"); err != nil {
		return nil, errors.New("docbank is not on PATH; install Docbank (https://github.com/kenn-io/docbank), then rerun")
	}
	out, err := exec.CommandContext(ctx, "docbank", args...).Output() //nolint:gosec // G204: fixed docbank subcommands from setup; no shell.
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		err = fmt.Errorf("docbank %s: %s", strings.Join(args, " "), strings.TrimSpace(string(exit.Stderr)))
	}
	return out, err
}

// docbankSection is the [docbank] table setup appends; credentials stay in the key file.
type docbankSection struct {
	URL        string `toml:"url"`
	APIKeyFile string `toml:"api_key_file"`
	Folder     string `toml:"folder"`
}

// docbankServer is the [server] table of Docbank's config.toml that fixes its address and key.
type docbankServer struct {
	BindAddr string `toml:"bind_addr"`
	APIPort  *int   `toml:"api_port"`
	APIKey   string `toml:"api_key"`
}

// localDocbank is what setup changes on this machine's Docbank, decided before anything is written.
type localDocbank struct {
	vault, configPath     string
	configText            []byte
	port                  int
	key                   string
	writeServer, writeKey bool
}

func newDocbankSetupCmd() *cobra.Command {
	var docbankURL, keyFile, folder string
	var port int
	var yes bool
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "point the daemon at a Docbank for attachments",
		Long: "Without --url, give the Docbank on this machine a fixed loopback port and API key, " +
			"store the key in <KATA_HOME>/docbank.key and restart Docbank. With --url and --key-file, " +
			"use a Docbank that already runs elsewhere. Either way, check the key against Docbank, " +
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
				return &cliError{Message: "--key-file goes with --url; without --url setup makes the key itself", Kind: kindValidation, ExitCode: ExitValidation}
			case !local && keyFile == "":
				return &cliError{Message: "--url needs --key-file naming a private file that holds the Docbank API key", Kind: kindValidation, ExitCode: ExitValidation}
			case !local && port != 0:
				return &cliError{Message: "--port sets up a Docbank on this machine and does not go with --url", Kind: kindValidation, ExitCode: ExitValidation}
			case port < 0 || port > 65535:
				return &cliError{Message: "--port must be between 1 and 65535", Kind: kindValidation, ExitCode: ExitValidation}
			}
			path, err := config.DaemonConfigPath()
			if err != nil {
				return err
			}
			var plan localDocbank
			if local {
				keyFile = filepath.Join(filepath.Dir(path), "docbank.key")
			}
			if keyFile != "~" && !strings.HasPrefix(keyFile, "~/") {
				if keyFile, err = filepath.Abs(keyFile); err != nil {
					return err
				}
			}
			if local {
				if plan, err = planLocalDocbank(ctx, port, keyFile); err != nil {
					return err
				}
				docbankURL = "http://127.0.0.1:" + strconv.Itoa(plan.port)
			}
			want, err := config.NormalizeDocbankConfig(config.DocbankConfig{URL: docbankURL, APIKeyFile: keyFile, Folder: folder})
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
			set := have != config.DocbankConfig{}
			if set {
				if have, err = config.NormalizeDocbankConfig(have); err != nil || have != want {
					return fmt.Errorf("%s already has a different [docbank] section; edit or remove it, then rerun", path)
				}
			}
			if !local {
				if err := checkDocbankKey(ctx, want); err != nil {
					return err
				}
			}
			changed := !set || plan.writeServer || plan.writeKey
			if changed {
				vault := want.URL
				if local {
					vault = plan.vault
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Every issue reader and metadata writer served by this daemon can read any file in this Docbank vault: %s\n", vault); err != nil {
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
			if local {
				if err := plan.apply(ctx, keyFile); err != nil {
					return err
				}
				if err := checkDocbankKey(ctx, want); err != nil {
					return err
				}
			}
			if !set {
				proposed, err := appendTOMLTable(original, "docbank", docbankSection{URL: want.URL, APIKeyFile: want.APIKeyFile, Folder: want.Folder})
				if err != nil {
					return fmt.Errorf("cannot add [docbank] to %s: %w", path, err)
				}
				if written, err := config.ParseDocbankFile(proposed); err != nil || written != (config.DocbankConfig{URL: want.URL, APIKeyFile: want.APIKeyFile, Folder: want.Folder}) {
					return fmt.Errorf("cannot add [docbank] to %s: the file already sets docbank keys", path)
				}
				if err := writeDaemonConfigAndRestart(ctx, cmd.ErrOrStderr(), path, original, originalExists, proposed); err != nil {
					return err
				}
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Service environment settings can override this configuration."); err != nil {
					return err
				}
			}
			return printDocbankSetup(cmd.OutOrStdout(), want, changed)
		},
	}
	cmd.Flags().StringVar(&docbankURL, "url", "", "use a Docbank running elsewhere; HTTPS except on loopback")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "private file holding that Docbank's API key (with --url)")
	cmd.Flags().IntVar(&port, "port", 0, "loopback port for the Docbank on this machine (default 8080)")
	cmd.Flags().StringVar(&folder, "folder", "", "Docbank folder for uploads (default /kata)")
	cmd.Flags().BoolVar(&yes, "yes", false, "approve the configuration change and restarts")
	return cmd
}

func printDocbankSetup(w io.Writer, want config.DocbankConfig, changed bool) error {
	var err error
	switch currentOutputMode() {
	case outputJSON:
		return emitJSON(w, struct {
			URL     string `json:"url"`
			Folder  string `json:"folder"`
			KeyFile string `json:"key_file"`
			Changed bool   `json:"changed"`
		}{want.URL, want.Folder, want.APIKeyFile, changed})
	case outputAgent:
		_, err = fmt.Fprintf(w, "OK docbank-setup url=%s folder=%s changed=%t\n", agentValue(want.URL), agentValue(want.Folder), changed)
	case outputHuman:
		if changed {
			_, err = fmt.Fprintf(w, "Docbank attachments use %s, folder %s.\n", want.URL, want.Folder)
		} else {
			_, err = fmt.Fprintf(w, "Docbank attachments are already set up: %s, folder %s.\n", want.URL, want.Folder)
		}
	}
	return err
}

// checkDocbankKey reads Docbank's root folder with the key the daemon will resolve from the same section.
func checkDocbankKey(ctx context.Context, want config.DocbankConfig) error {
	key, err := want.ResolveKey()
	if err != nil {
		return err
	}
	_, err = docbank.NewClient(want.URL, func() (string, error) { return key, nil }).ResolvePath(ctx, "/")
	status, ok := errors.AsType[*docbank.Error](err)
	switch {
	case err == nil:
		return nil
	case ok && (status.Status == 401 || status.Status == 403):
		return fmt.Errorf("docbank at %s rejected the API key (HTTP %d); kata's config was not changed", want.URL, status.Status)
	case ok && status.Status != 0:
		return fmt.Errorf("docbank at %s answered HTTP %d to a read of its root folder; kata's config was not changed", want.URL, status.Status)
	default:
		return fmt.Errorf("cannot reach Docbank at %s; kata's config was not changed", want.URL)
	}
}

// planLocalDocbank finds this machine's Docbank and decides its port and key without writing anything.
func planLocalDocbank(ctx context.Context, port int, keyFile string) (localDocbank, error) {
	out, err := runDocbank(ctx, "vault", "info", "--json")
	if err != nil {
		return localDocbank{}, err
	}
	var info struct {
		VaultPath string `json:"vault_path"`
	}
	if json.Unmarshal(out, &info) != nil || info.VaultPath == "" {
		return localDocbank{}, errors.New("docbank vault info --json did not name the vault")
	}
	plan := localDocbank{vault: info.VaultPath, configPath: filepath.Join(info.VaultPath, "config.toml")}
	if plan.configText, _, err = readDaemonConfig(plan.configPath); err != nil {
		return localDocbank{}, err
	}
	var file struct {
		Server *docbankServer `toml:"server"`
	}
	if _, err := toml.Decode(string(plan.configText), &file); err != nil {
		return localDocbank{}, fmt.Errorf("read %s: %w", plan.configPath, err)
	}
	stored, err := os.ReadFile(keyFile) // #nosec G304 -- the key file under the operator's KATA_HOME
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return localDocbank{}, fmt.Errorf("read %s: %w", keyFile, err)
	}
	diskKey := strings.TrimSpace(string(stored))
	if server := file.Server; server != nil {
		if server.APIPort == nil || server.APIKey == "" {
			return localDocbank{}, fmt.Errorf("%s has a [server] section without both api_port and api_key; add the missing one or remove the section, then rerun", plan.configPath)
		}
		if plan.port, plan.key = *server.APIPort, server.APIKey; plan.port < 1 || plan.port > 65535 {
			return localDocbank{}, fmt.Errorf("%s: api_port %d is not a port", plan.configPath, plan.port)
		}
		if port != 0 && port != plan.port {
			return localDocbank{}, fmt.Errorf("--port %d differs from api_port %d in %s; drop --port or edit that file", port, plan.port, plan.configPath)
		}
		if diskKey != "" && diskKey != plan.key {
			return localDocbank{}, fmt.Errorf("%s holds a different key than %s; remove it or give Docbank that key, then rerun", keyFile, plan.configPath)
		}
	} else {
		plan.writeServer = true
		if plan.port = port; plan.port == 0 {
			plan.port = defaultDocbankPort
		}
		if plan.key = diskKey; plan.key == "" {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return localDocbank{}, err
			}
			plan.key = hex.EncodeToString(raw)
		}
	}
	plan.writeKey = diskKey == ""
	return plan, nil
}

// apply fixes Docbank's port and key, stores the key for kata, and restarts Docbank when it is not
// already serving that port, then requires Docbank to report that address.
func (p localDocbank) apply(ctx context.Context, keyFile string) error {
	addr := "127.0.0.1:" + strconv.Itoa(p.port)
	names := regexp.MustCompile(regexp.QuoteMeta(addr) + `\b`)
	status, _ := runDocbank(ctx, "daemon", "status") // a stopped Docbank exits non-zero
	running := names.Match(status)
	if !running {
		listener, err := new(net.ListenConfig).Listen(ctx, "tcp", addr)
		if err != nil {
			return fmt.Errorf("port %d on 127.0.0.1 is in use by something other than this Docbank; free it or pick another with --port", p.port)
		}
		_ = listener.Close()
	}
	if p.writeKey {
		if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
			return err
		}
		if err := atomicfile.WriteFile(keyFile, []byte(p.key+"\n"), atomicfile.WithPrivate()); err != nil {
			return err
		}
	}
	if p.writeServer {
		latest, _, err := readDaemonConfig(p.configPath)
		if err != nil {
			return err
		}
		if !bytes.Equal(latest, p.configText) {
			return fmt.Errorf("%s changed during setup; rerun the command", p.configPath)
		}
		port := p.port
		text, err := appendTOMLTable(p.configText, "server", docbankServer{BindAddr: "127.0.0.1", APIPort: &port, APIKey: p.key})
		if err != nil {
			return err
		}
		if err := os.MkdirAll(p.vault, 0o700); err != nil {
			return err
		}
		if err := atomicfile.WriteFile(p.configPath, text, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink()); err != nil {
			return err
		}
	}
	if running && !p.writeServer {
		return nil
	}
	if _, err := runDocbank(ctx, "daemon", "restart"); err != nil {
		return fmt.Errorf("%w; kata's config was not changed", err)
	}
	if status, _ = runDocbank(ctx, "daemon", "status"); !names.Match(status) {
		return fmt.Errorf("docbank did not come up at %s (DOCBANK_BIND_ADDR or DOCBANK_API_PORT in its environment override its config); kata's config was not changed. docbank daemon status says: %s", addr, strings.TrimSpace(string(status)))
	}
	return nil
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
