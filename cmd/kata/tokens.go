package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	"go.kenn.io/kata/internal/tokenfile"
	"go.kenn.io/kata/internal/tokenissuance"
)

type tokenScopeCLIOut struct {
	Kind         string `json:"kind"`
	ProjectUID   string `json:"project_uid"`
	RootIssueUID string `json:"root_issue_uid"`
}

type tokenCLIOut struct {
	ID         int64             `json:"id"`
	Actor      string            `json:"actor"`
	Name       *string           `json:"name"`
	Scope      *tokenScopeCLIOut `json:"scope,omitempty"`
	ExpiresAt  *time.Time        `json:"expires_at,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	LastUsedAt *time.Time        `json:"last_used_at"`
	RevokedAt  *time.Time        `json:"revoked_at"`
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
	var actor, name, issue, expiresIn, tokenFile string
	cmd := &cobra.Command{
		Use:   "create --actor <actor> [--name <name>] [--issue <ref> --expires-in <duration> --token-file <path>]",
		Short: "create an identity token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			actor = strings.TrimSpace(actor)
			if actor == "" {
				return &cliError{Message: "actor is required", Kind: kindUsage, ExitCode: ExitUsage}
			}
			scoped := strings.TrimSpace(issue) != "" || strings.TrimSpace(expiresIn) != "" || strings.TrimSpace(tokenFile) != ""
			if scoped {
				return createScopedToken(cmd, actor, name, issue, expiresIn, tokenFile)
			}
			a, err := dialDaemon(cmd.Context())
			if err != nil {
				return err
			}
			payload := map[string]string{"actor": actor}
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				payload["name"] = trimmed
			}
			bs, emitted, err := a.passthrough(cmd, http.MethodPost, "/api/v1/tokens", payload)
			if err != nil || emitted {
				return err
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
	cmd.Flags().StringVar(&issue, "issue", "", "limit the token to this issue subtree")
	cmd.Flags().StringVar(&expiresIn, "expires-in", "", "required lifetime for a scoped token")
	cmd.Flags().StringVar(&tokenFile, "token-file", "", "new owner-only file for scoped token plaintext")
	return cmd
}

func createScopedToken(cmd *cobra.Command, actor, name, issue, expiresIn, tokenPath string) (retErr error) {
	issue = strings.TrimSpace(issue)
	expiresIn = strings.TrimSpace(expiresIn)
	tokenPath = strings.TrimSpace(tokenPath)
	if issue == "" || expiresIn == "" || tokenPath == "" {
		return &cliError{
			Message:  "scoped token creation requires --issue, --expires-in, and --token-file together",
			Kind:     kindUsage,
			Code:     "scoped_token_arguments_required",
			ExitCode: ExitUsage,
		}
	}
	duration, err := time.ParseDuration(expiresIn)
	if err != nil || duration <= 0 || duration < time.Second || duration%time.Second != 0 {
		return &cliError{
			Message:  "--expires-in must be a positive whole-second duration",
			Kind:     kindValidation,
			Code:     "invalid_token_expiration",
			ExitCode: ExitValidation,
		}
	}
	a, err := dialDaemon(cmd.Context())
	if err != nil {
		return err
	}
	var instance struct {
		IssueSubtreeTokens bool `json:"issue_subtree_tokens"`
	}
	if err := a.decode(http.MethodGet, "/api/v1/instance", nil, &instance); err != nil {
		return err
	}
	if !instance.IssueSubtreeTokens {
		return &cliError{
			Message:  "the selected daemon does not support issue-scoped tokens",
			Kind:     kindConflict,
			Code:     "issue_subtree_tokens_unsupported",
			ExitCode: ExitConflict,
		}
	}
	scope, qualifiedIssue, err := resolveScopedTokenIssue(cmd, a, issue)
	if err != nil {
		return err
	}
	reservation, err := tokenfile.Reserve(tokenPath)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reservation.Abort()) }()

	payload := map[string]any{
		"actor": actor,
		"scope": map[string]string{
			"kind":           scope.Kind,
			"project_uid":    scope.ProjectUID,
			"root_issue_uid": scope.RootIssueUID,
		},
		"expires_in_seconds": int64(duration / time.Second),
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		payload["name"] = trimmed
	}
	bs, err := a.do(http.MethodPost, "/api/v1/tokens", payload)
	if err != nil {
		if _, ok := errors.AsType[*cliError](err); !ok {
			return fmt.Errorf("scoped token creation response was lost; outcome is ambiguous: %w", err)
		}
		return err
	}
	var response createTokenCLIResponse
	if err := json.Unmarshal(bs, &response); err != nil {
		return fmt.Errorf("scoped token creation response could not be decoded; outcome is ambiguous: %w", err)
	}
	responseScope := (*tokenissuance.Scope)(nil)
	if response.Token.Scope != nil {
		responseScope = &tokenissuance.Scope{
			Kind: response.Token.Scope.Kind, ProjectUID: response.Token.Scope.ProjectUID,
			RootIssueUID: response.Token.Scope.RootIssueUID,
		}
	}
	if err := tokenissuance.Validate(tokenissuance.Response{
		ID: response.Token.ID, Actor: response.Token.Actor, Scope: responseScope,
		CreatedAt: response.Token.CreatedAt, ExpiresAt: response.Token.ExpiresAt,
		Plaintext: response.Plaintext,
	}, actor, tokenissuance.Scope{
		Kind: scope.Kind, ProjectUID: scope.ProjectUID, RootIssueUID: scope.RootIssueUID,
	}, duration); err != nil {
		return cleanupScopedTokenCreation(a, response.Token.ID, err)
	}
	if err := reservation.Commit(response.Plaintext); err != nil {
		return cleanupScopedTokenCreation(a, response.Token.ID, err)
	}
	return printScopedTokenCreated(cmd, response.Token, qualifiedIssue, reservation.Path())
}

func resolveScopedTokenIssue(_ *cobra.Command, a daemonAPI, raw string) (tokenScopeCLIOut, string, error) {
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return tokenScopeCLIOut{}, "", err
	}
	fallback := strings.TrimSpace(flags.Project)
	if fallback == "" {
		fallback = workspaceProjectName(start)
	}
	parsed, err := ResolveRef(raw, fallback)
	if err != nil {
		return tokenScopeCLIOut{}, "", &cliError{Message: err.Error(), Kind: kindValidation, ExitCode: ExitValidation}
	}
	if explicit := strings.TrimSpace(flags.Project); explicit != "" && parsed.ProjectName != explicit {
		return tokenScopeCLIOut{}, "", &cliError{
			Message:  fmt.Sprintf("--project %q conflicts with issue project %q", explicit, parsed.ProjectName),
			Kind:     kindValidation,
			Code:     "conflicting_project_selector",
			ExitCode: ExitValidation,
		}
	}
	projectID, projectName, err := resolveProjectIDAndNameForRef(a, start, parsed.ProjectName, false)
	if err != nil {
		return tokenScopeCLIOut{}, "", err
	}
	var shown struct {
		Issue struct {
			UID        string `json:"uid"`
			ProjectUID string `json:"project_uid"`
			ShortID    string `json:"short_id"`
		} `json:"issue"`
	}
	path := fmt.Sprintf("/api/v1/projects/%d/issues/%s", projectID, url.PathEscape(parsed.RefForAPI))
	if err := a.decode(http.MethodGet, path, nil, &shown); err != nil {
		return tokenScopeCLIOut{}, "", err
	}
	if shown.Issue.UID == "" || shown.Issue.ProjectUID == "" || shown.Issue.ShortID == "" {
		return tokenScopeCLIOut{}, "", errors.New("daemon returned incomplete issue identity for scoped token creation")
	}
	return tokenScopeCLIOut{
		Kind: "issue_subtree", ProjectUID: shown.Issue.ProjectUID, RootIssueUID: shown.Issue.UID,
	}, projectName + "#" + shown.Issue.ShortID, nil
}

func cleanupScopedTokenCreation(a daemonAPI, tokenID int64, cause error) error {
	if tokenID <= 0 {
		return fmt.Errorf("scoped token delivery failed; creation outcome is ambiguous: %w", cause)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 10*time.Second)
	defer cancel()
	a.ctx = cleanupCtx
	path := fmt.Sprintf("/api/v1/tokens/%d/actions/revoke", tokenID)
	if _, err := a.do(http.MethodPost, path, nil); err != nil {
		return fmt.Errorf("scoped token delivery failed and token %d could not be revoked: %v: %w", tokenID, err, cause)
	}
	return fmt.Errorf("scoped token delivery failed; token %d was revoked: %w", tokenID, cause)
}

func printScopedTokenCreated(cmd *cobra.Command, token tokenCLIOut, issue, path string) error {
	if currentOutputMode() == outputJSON {
		return emitJSON(cmd.OutOrStdout(), struct {
			Token     tokenCLIOut `json:"token"`
			Issue     string      `json:"issue"`
			TokenFile string      `json:"token_file"`
		}{Token: token, Issue: issue, TokenFile: path})
	}
	if currentOutputMode() == outputAgent {
		return writeAgentKVRow(cmd.OutOrStdout(),
			agentRowField("id", strconv.FormatInt(token.ID, 10)),
			agentRowField("actor", token.Actor),
			agentRowField("issue", issue),
			agentRowField("expires_at", token.ExpiresAt.UTC().Format(time.RFC3339)),
			agentRowField("token_file", path),
		)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(),
		"created scoped token id=%d actor=%s issue=%s expires_at=%s token_file=%s\n",
		token.ID, textsafe.Line(token.Actor), textsafe.Line(issue),
		token.ExpiresAt.UTC().Format(time.RFC3339), textsafe.Line(path))
	return err
}

func tokensListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list identity tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := dialDaemon(cmd.Context())
			if err != nil {
				return err
			}
			bs, emitted, err := a.passthrough(cmd, http.MethodGet, "/api/v1/tokens", nil)
			if err != nil || emitted {
				return err
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
			a, err := dialDaemon(cmd.Context())
			if err != nil {
				return err
			}
			path := fmt.Sprintf("/api/v1/tokens/%d/actions/revoke", id)
			bs, emitted, err := a.passthrough(cmd, http.MethodPost, path, nil)
			if err != nil || emitted {
				return err
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
			if tok.Scope != nil {
				fields = append(fields,
					agentRowField("scope", tok.Scope.Kind),
					agentRowField("project_uid", tok.Scope.ProjectUID),
					agentRowField("root_issue_uid", tok.Scope.RootIssueUID),
				)
			}
			if tok.ExpiresAt != nil {
				fields = append(fields, agentRowField("expires_at", tok.ExpiresAt.UTC().Format(time.RFC3339)))
			}
			if err := writeAgentKVRow(cmd.OutOrStdout(), fields...); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "id  actor  name  scope  project_uid  root_issue_uid  expires_at  revoked"); err != nil {
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
		scopeKind, projectUID, rootIssueUID, expiresAt := "", "", "", ""
		if tok.Scope != nil {
			scopeKind = tok.Scope.Kind
			projectUID = tok.Scope.ProjectUID
			rootIssueUID = tok.Scope.RootIssueUID
		}
		if tok.ExpiresAt != nil {
			expiresAt = tok.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d  %s  %s  %s  %s  %s  %s  %s\n",
			tok.ID, textsafe.Line(tok.Actor), textsafe.Line(name), textsafe.Line(scopeKind),
			textsafe.Line(projectUID), textsafe.Line(rootIssueUID), textsafe.Line(expiresAt),
			textsafe.Line(revoked)); err != nil {
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
