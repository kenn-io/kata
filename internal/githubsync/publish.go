package githubsync

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/issuesync"
)

// NewPublishAdapter uses the same repository-scoped transport as normal sync.
func NewPublishAdapter(fetcher Fetcher) issuesync.PublishAdapter {
	return &adapter{config: RunnerConfig{Fetcher: fetcher}}
}

func (a *adapter) OpenPublish(ctx context.Context, b db.IssueSyncBinding) (issuesync.PublishSession, error) {
	c, err := DecodeConfig(b.Config)
	if err != nil {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	if b.Provider != "github" || b.RemoteID == "" || b.SourceKey != "github:"+b.RemoteID || c.RepoID <= 0 {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationBindingInvalid)
	}
	fetcher, err := a.fetcherForBinding(ctx, c.Binding())
	if err != nil {
		return nil, err
	}
	session, ok := fetcher.(*httpFetcherBindingSession)
	if !ok {
		return nil, fmt.Errorf("GitHub session does not support publishing")
	}
	return &githubPublishSession{session: session, config: c, nodeID: b.RemoteID}, nil
}

type githubPublishSession struct {
	session *httpFetcherBindingSession
	config  Config
	nodeID  string
}
type githubCreatePayload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (s *githubPublishSession) verifyRepository(ctx context.Context, creating bool) error {
	var repo struct {
		Repository
		HasIssues bool `json:"has_issues"`
		Archived  bool `json:"archived"`
	}
	if err := s.session.statusRequest(ctx, http.MethodGet, repositoryEndpoint(s.session.binding), nil, &repo, nil); err != nil {
		return err
	}
	if repo.ID != s.config.RepoID || repo.NodeID != s.nodeID || !strings.EqualFold(repo.FullName, s.config.DisplayName()) {
		return issuesync.NewPublicationValidationError(issuesync.PublicationDestinationUnavailable)
	}
	if creating && (!repo.HasIssues || repo.Archived) {
		return issuesync.NewPublicationValidationError(issuesync.PublicationDestinationUnavailable)
	}
	return nil
}
func (s *githubPublishSession) PrepareCreate(ctx context.Context, issue db.Issue) (jsontext.Value, error) {
	payload := githubCreatePayload{Title: issue.Title, Body: issue.Body}
	if issue.Status != "open" {
		return nil, issuesync.NewPublicationValidationError(issuesync.PublicationOpenIssueRequired)
	}
	if err := validateGitHubCreate(payload); err != nil {
		return nil, err
	}
	if err := s.verifyRepository(ctx, true); err != nil {
		return nil, err
	}
	return json.Marshal(payload)
}
func validateGitHubCreate(p githubCreatePayload) error {
	if strings.TrimSpace(p.Title) == "" || !utf8.ValidString(p.Title) || !utf8.ValidString(p.Body) || utf8.RuneCountInString(p.Title) > 256 || utf8.RuneCountInString(p.Body) > 65536 {
		return issuesync.NewPublicationValidationError(issuesync.PublicationGitHubContentInvalid)
	}
	return nil
}
func (s *githubPublishSession) Create(ctx context.Context, payload jsontext.Value, admit func(context.Context) error) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	var body githubCreatePayload
	if err := json.Unmarshal(payload, &body, json.RejectUnknownMembers(true)); err != nil {
		return zero, fmt.Errorf("invalid GitHub creation payload")
	}
	if err := validateGitHubCreate(body); err != nil {
		return zero, err
	}
	if admit == nil {
		return zero, fmt.Errorf("GitHub creation requires admission")
	}
	if err := s.verifyRepository(ctx, true); err != nil {
		return zero, err
	}
	var wire statusIssue
	if err := s.session.statusRequest(ctx, http.MethodPost, repositoryEndpoint(s.session.binding)+"/issues", body, &wire, func() error { return admit(ctx) }); err != nil {
		return zero, err
	}
	return s.observation(wire, wire.ID, wire.Number)
}
func (s *githubPublishSession) ReadCreated(ctx context.Context, identity db.IssuePublicationRemote) (db.IssuePublicationRemote, error) {
	var zero db.IssuePublicationRemote
	numberText := identity.ExternalID
	var expectedID int64
	if strings.HasPrefix(identity.ExternalID, "issue-id:") {
		var err error
		expectedID, err = strconv.ParseInt(strings.TrimPrefix(identity.ExternalID, "issue-id:"), 10, 64)
		if err != nil || expectedID <= 0 || identity.ExternalID != "issue-id:"+strconv.FormatInt(expectedID, 10) {
			return zero, fmt.Errorf("invalid GitHub issue identity")
		}
		numberText = identity.Locator
	}
	number, err := strconv.Atoi(numberText)
	if err != nil || number <= 0 || strconv.Itoa(number) != numberText {
		return zero, fmt.Errorf("GitHub adoption requires a repository issue number")
	}
	if err := s.verifyRepository(ctx, false); err != nil {
		return zero, err
	}
	var wire statusIssue
	if err := s.session.statusRequest(ctx, http.MethodGet, repositoryEndpoint(s.session.binding)+"/issues/"+numberText, nil, &wire, nil); err != nil {
		return zero, err
	}
	if expectedID == 0 {
		expectedID = wire.ID
	}
	return s.observation(wire, expectedID, number)
}
func (s *githubPublishSession) observation(wire statusIssue, id int64, number int) (db.IssuePublicationRemote, error) {
	var remote db.IssuePublicationRemote
	if err := s.session.validateStatusIdentity(wire, id, number); err != nil {
		return remote, err
	}
	remote.ExternalID = "issue-id:" + strconv.FormatInt(id, 10)
	remote.LegacyExternalIDs = issueLegacyExternalIDs(wire.Issue)
	remote.Locator = strconv.Itoa(number)
	remote.URL = "https://" + s.session.binding.Host + "/" + s.session.binding.Owner + "/" + s.session.binding.Repo + "/issues/" + remote.Locator
	if err := s.session.validateStatusIssue(wire, id, number); err != nil {
		return remote, err
	}
	remote.Status = wire.State
	remote.RawStatus = &wire.State
	remote.Version = wire.UpdatedAt.UTC()
	remote.SourceUpdatedAt = &remote.Version
	if wire.State == "closed" {
		remote.ClosedReason = issueClosedReason(wire.Issue)
	}
	return remote, nil
}

var _ issuesync.PublishAdapter = (*adapter)(nil)
var _ issuesync.PublishSession = (*githubPublishSession)(nil)
