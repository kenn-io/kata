package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
)

type tokenCLIOut struct {
	ID         int64      `json:"id"`
	Actor      string     `json:"actor"`
	Name       *string    `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

type createTokenCLIResponse struct {
	Token     tokenCLIOut `json:"token"`
	Plaintext string      `json:"plaintext"`
}

type listTokensCLIResponse struct {
	Tokens []tokenCLIOut `json:"tokens"`
}

type revokeTokenCLIResponse struct {
	Token tokenCLIOut `json:"token"`
}

func newTokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "manage daemon identity tokens",
	}
	cmd.AddCommand(tokensCreateCmd(), tokensListCmd(), tokensRevokeCmd())
	return cmd
}

func tokensCreateCmd() *cobra.Command {
	var actor, name string
	cmd := &cobra.Command{
		Use:   "create --actor <actor> [--name <name>]",
		Short: "create an identity token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor = strings.TrimSpace(actor)
			if actor == "" {
				return &cliError{Message: "actor is required", Kind: kindUsage, ExitCode: ExitUsage}
			}
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			payload := map[string]string{"actor": actor}
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				payload["name"] = trimmed
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost, baseURL+"/api/v1/tokens", payload)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if currentOutputMode() == outputJSON {
				return emitJSON(cmd.OutOrStdout(), json.RawMessage(bs))
			}
			var out createTokenCLIResponse
			if err := json.Unmarshal(bs, &out); err != nil {
				return err
			}
			return printTokenCreated(cmd, out)
		},
	}
	cmd.Flags().StringVar(&actor, "actor", "", "actor this token identifies")
	cmd.Flags().StringVar(&name, "name", "", "human label for the token")
	return cmd
}

func tokensListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list identity tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			status, bs, err := httpDoJSON(ctx, client, http.MethodGet, baseURL+"/api/v1/tokens", nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if currentOutputMode() == outputJSON {
				return emitJSON(cmd.OutOrStdout(), json.RawMessage(bs))
			}
			var out listTokensCLIResponse
			if err := json.Unmarshal(bs, &out); err != nil {
				return err
			}
			return printTokensList(cmd, out)
		},
	}
}

func tokensRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <id>",
		Short: "revoke an identity token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseTokenID(args[0])
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			baseURL, err := ensureDaemon(ctx)
			if err != nil {
				return err
			}
			client, err := httpClientFor(ctx, baseURL)
			if err != nil {
				return err
			}
			url := fmt.Sprintf("%s/api/v1/tokens/%d/actions/revoke", baseURL, id)
			status, bs, err := httpDoJSON(ctx, client, http.MethodPost, url, nil)
			if err != nil {
				return err
			}
			if status >= 400 {
				return apiErrFromBody(status, bs)
			}
			if currentOutputMode() == outputJSON {
				return emitJSON(cmd.OutOrStdout(), json.RawMessage(bs))
			}
			var out revokeTokenCLIResponse
			if err := json.Unmarshal(bs, &out); err != nil {
				return err
			}
			return printTokenRevoked(cmd, out)
		},
	}
}

func parseTokenID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, &cliError{Message: "token id must be a positive integer", Kind: kindUsage, ExitCode: ExitUsage}
	}
	return id, nil
}

func printTokenCreated(cmd *cobra.Command, out createTokenCLIResponse) error {
	if currentOutputMode() == outputAgent {
		if !flags.Quiet {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK token_created id=%d actor=%s",
				out.Token.ID, agentValue(out.Token.Actor)); err != nil {
				return err
			}
			if out.Token.Name != nil {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), " name=%s", agentValue(*out.Token.Name)); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
				return err
			}
		}
		return writeAgentField(cmd.OutOrStdout(), "Token", agentValue(out.Plaintext))
	}
	if out.Token.Name != nil {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "created token id=%d actor=%s name=%s\n",
			out.Token.ID, textsafe.Line(out.Token.Actor), textsafe.Line(*out.Token.Name)); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "created token id=%d actor=%s\n",
			out.Token.ID, textsafe.Line(out.Token.Actor)); err != nil {
			return err
		}
	}
	if !flags.Quiet {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Copy this token now. It will not be shown again."); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Store it in the user's client config.toml or environment."); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "token=%s\n", textsafe.Line(out.Plaintext))
	return err
}

func printTokensList(cmd *cobra.Command, out listTokensCLIResponse) error {
	if currentOutputMode() == outputAgent {
		if !flags.Quiet {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK tokens count=%d\n", len(out.Tokens)); err != nil {
				return err
			}
		}
		for _, tok := range out.Tokens {
			fields := []agentField{
				agentRowField("id", strconv.FormatInt(tok.ID, 10)),
				agentRowField("actor", tok.Actor),
				agentOptionalRowField("name", tok.Name),
				agentRowField("revoked", strconv.FormatBool(tok.RevokedAt != nil)),
			}
			if err := writeAgentKVRow(cmd.OutOrStdout(), fields...); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "id  actor  name  revoked"); err != nil {
		return err
	}
	for _, tok := range out.Tokens {
		name := ""
		if tok.Name != nil {
			name = *tok.Name
		}
		revoked := ""
		if tok.RevokedAt != nil {
			revoked = tok.RevokedAt.Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d  %s  %s  %s\n",
			tok.ID, textsafe.Line(tok.Actor), textsafe.Line(name), textsafe.Line(revoked)); err != nil {
			return err
		}
	}
	return nil
}

func printTokenRevoked(cmd *cobra.Command, out revokeTokenCLIResponse) error {
	if currentOutputMode() == outputAgent {
		if !flags.Quiet {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "OK token_revoked id=%d actor=%s\n",
				out.Token.ID, agentValue(out.Token.Actor)); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "revoked token id=%d actor=%s\n",
		out.Token.ID, textsafe.Line(out.Token.Actor))
	return err
}
