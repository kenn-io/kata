package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.kenn.io/kata/internal/api"
	clientpkg "go.kenn.io/kata/internal/client"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/cron"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/federationcoord"
	"go.kenn.io/kata/internal/federationsigning"
	"go.kenn.io/kata/internal/httpurl"
)

func cronForwardUnavailable(message string) error {
	return api.NewError(503, "federation_offline", message, "Retry shortly, or run the command against the owning hub with --daemon <hub-name>.", nil)
}

// forwardCronJob never applies hub response events locally. Portable event UIDs
// are authoritative; numeric projection identities become available after pull.
func forwardCronJob(ctx context.Context, cfg ServerConfig, projectID int64, uid, method, suffix string, put *api.PutCronJobBody, action *api.CronDefinitionActionBody, enrollment bool) (*api.CronJobResponse, bool, error) {
	if enrollment {
		return nil, false, nil
	}
	native := put != nil && cron.IsRecurringIssueJob(put.Definition.Native())
	if uid != "" {
		old, err := cfg.DB.CronJob(ctx, projectID, uid)
		if err == nil {
			native = native || cron.IsRecurringIssueJob(old.Definition)
		} else if !errors.Is(err, db.ErrNotFound) {
			return nil, true, cronAPIError(ctx, cfg, err, projectID, uid, true)
		} else if action != nil {
			native = true
		}
	}
	if !native {
		return nil, false, nil
	}
	binding, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if errors.Is(err, db.ErrNotFound) || (err == nil && binding.Role != db.FederationRoleSpoke) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, internalAPIError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	release, err := federationcoord.TryBeginSync(ctx, federationcoord.Key(cfg.DB.InstanceUID(), projectID), cfg.DB, projectID)
	if err != nil {
		return nil, true, cronForwardUnavailable("federation endpoint is busy; retry shortly")
	}
	defer release()
	// Reload before reading secret material. Rebind cannot pass this admission.
	binding, err = cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil {
		return nil, true, cronForwardUnavailable("federation binding is unavailable")
	}
	if binding.Role != db.FederationRoleSpoke || !binding.Enabled || !binding.PushEnabled || binding.RelayConfig != nil {
		return nil, true, federationReadOnlyError(errors.Join(db.ErrFederatedReadOnly, db.ErrRecurringHubRequired))
	}
	project, err := activeProjectByID(ctx, cfg.DB, projectID)
	if err != nil {
		return nil, true, err
	}
	cred, found, err := cfg.federationCredentialStore().FederationCredential(ctx, project.UID)
	if err != nil || !found || cred.Token == "" || cred.RelayEnrollmentPending || cred.LeavePending {
		return nil, true, cronForwardUnavailable("federation push credentials are unavailable")
	}
	credentialOrigin, e1 := httpurl.CanonicalHTTPBaseURL(cred.HubURL)
	bindingOrigin, e2 := httpurl.CanonicalHTTPBaseURL(binding.HubURL)
	if e1 != nil || e2 != nil || credentialOrigin != bindingOrigin || cred.HubProjectID != binding.HubProjectID {
		return nil, true, cronForwardUnavailable("federation credential target differs from the binding; reconnect before retrying")
	}
	cred = config.FederationTransportCredential(binding.HubURL, binding.HubProjectID, binding.AllowInsecure, cred)
	client, err := clientpkg.NewHTTPClientWithBearer(ctx, cred.HubURL, cred.Token, clientpkg.Opts{Timeout: 15 * time.Second, AllowInsecure: cred.AllowInsecure})
	if err != nil {
		return nil, true, cronForwardUnavailable("federation transport is unavailable")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if cred.Signing != nil {
		if err := federationsigning.ConfigureClient(client, cred.HubURL, cred.Token, *cred.Signing); err != nil {
			return nil, true, cronForwardUnavailable("federation signing is unavailable")
		}
	}
	path := fmt.Sprintf("%s/api/v1/projects/%d", bindingOrigin, binding.HubProjectID)
	var meta api.ProjectFederationBody
	if err := cronHubRequest(ctx, client, http.MethodGet, path+"/federation/metadata", nil, &meta, projectID); err != nil {
		return nil, true, err
	}
	if !meta.NativeCronWrites {
		return nil, true, api.NewError(409, "federated_read_only", "owning hub does not support native cron writes", "Upgrade the hub, or run the command against it with --daemon <hub-name>.", nil)
	}
	if cfg.FederationPushPending == nil {
		return nil, true, cronForwardUnavailable("federation push transport is unavailable")
	}
	if err := cfg.FederationPushPending(ctx, binding, cred); err != nil {
		return nil, true, cronForwardUnavailable("pending federation push failed; no cron write was sent")
	}
	current, err := cfg.DB.FederationBindingByProject(ctx, projectID)
	if err != nil || !current.Enabled || !current.PushEnabled || current.Role != binding.Role || current.RelayConfig != nil || current.HubURL != binding.HubURL || current.HubProjectID != binding.HubProjectID || current.HubProjectUID != binding.HubProjectUID || current.AllowInsecure != binding.AllowInsecure {
		return nil, true, cronForwardUnavailable("federation binding changed; retry with fresh credentials")
	}
	if err := cronForwardAdmission(ctx, cfg.DB, project.ID, project.UID); err != nil {
		return nil, true, cronAPIError(ctx, cfg, err, projectID, uid, true)
	}
	var body any
	if put != nil {
		copy := *put
		copy.Actor = cred.Actor
		body = copy
	} else {
		copy := *action
		copy.Actor = cred.Actor
		body = copy
	}
	path += "/cron/jobs"
	if uid != "" {
		path += "/" + url.PathEscape(uid)
	}
	path += suffix
	out := &api.CronJobResponse{}
	if err := cronHubRequest(ctx, client, method, path, body, &out.Body, projectID); err != nil {
		return nil, true, err
	}
	if out.Body.Job.UID == "" || out.Body.Job.DefinitionEventUID == "" {
		return nil, true, cronForwardUnavailable("hub response is incomplete; outcome may be unknown; re-read the hub or retry with the same job UID")
	}
	localCronIdentity(&out.Body.Job, projectID)
	out.Body.Events = nil
	if cfg.FederationWake != nil {
		cfg.FederationWake()
	}
	return out, true, nil
}

func cronForwardAdmission(ctx context.Context, store db.Storage, projectID int64, projectUID string) error {
	beginner, ok := store.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return cronForwardUnavailable("local authorization transaction is unavailable")
	}
	tx, err := beginner.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// BeginTx in both storage backends already applies request and root fences.
	err = db.CheckProjectAccessTransaction(ctx, tx, projectUID)
	if err != nil {
		return db.FinishTransactionRollback(ctx, err, tx.Rollback())
	}
	// Owner authority bypasses membership checks, but never project liveness.
	// Recheck the original identity after the network preflight and pending push.
	var live int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=$1 AND uid=$2 AND deleted_at IS NULL`, projectID, projectUID).Scan(&live)
	if errors.Is(err, sql.ErrNoRows) {
		err = api.NewError(404, "project_not_found", "project not found", "", nil)
	}
	if err != nil {
		return db.FinishTransactionRollback(ctx, err, tx.Rollback())
	}
	return tx.Commit()
}

func localCronIdentity(job *api.CronJob, projectID int64) {
	job.ID = 0
	job.Revision = 0
	job.ProjectID = projectID
}

// A full accepted job may appear in cron_conflict.data.current. Use the same
// bulk bound as signed requests, rather than truncating structured errors.
func cronHubRequest(ctx context.Context, client *http.Client, method, path string, body, out any, projectID int64) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(db.EventFeaturesHeader, db.SupportedEventFeatures)
	response, err := client.Do(req)
	if err != nil {
		if method == http.MethodGet {
			return cronForwardUnavailable("hub metadata is unavailable")
		}
		return cronForwardUnavailable("hub write response was lost; outcome may be unknown; re-read the hub or retry with the same job UID")
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, federationsigning.MaxBodyBytes+1))
	if err != nil || len(raw) > federationsigning.MaxBodyBytes {
		return cronForwardUnavailable("hub response is incomplete; outcome may be unknown; re-read the hub or retry with the same job UID")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code    string                    `json:"code"`
				Message string                    `json:"message"`
				Hint    string                    `json:"hint"`
				Data    map[string]jsontext.Value `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
			data := map[string]any{}
			for key, value := range envelope.Error.Data {
				data[key] = value
			}
			if encoded, ok := envelope.Error.Data["current"]; ok {
				var current api.CronJob
				if json.Unmarshal(encoded, &current) == nil {
					localCronIdentity(&current, projectID)
					data["current"] = current
				}
			}
			return api.NewError(response.StatusCode, envelope.Error.Code, envelope.Error.Message, envelope.Error.Hint, data)
		}

		return cronForwardUnavailable(fmt.Sprintf("hub returned status %d; outcome may be unknown", response.StatusCode))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return cronForwardUnavailable("hub response is invalid; outcome may be unknown; re-read the hub or retry with the same job UID")
	}
	return nil
}
