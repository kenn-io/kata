package main

import (
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/kata/internal/textsafe"
	kataclient "go.kenn.io/kata/pkg/client"
	"go.kenn.io/kata/pkg/client/generated"
)

type issueIdentityForOutput struct {
	ProjectID int64  `json:"project_id"`
	UID       string `json:"uid"`
	ShortID   string `json:"short_id"`
}

func issueOutputFallback(issue issueIdentityForOutput, err error) (string, error) {
	if issue.UID != "" {
		return textsafe.Line(issue.UID), nil
	}
	return "", err
}

// issueRefForOutput keeps references usable in the requested project context.
// Display lookup failures after a committed write fall back to stable identity.
func issueRefForOutput(a daemonAPI, requestedID int64, requestedName string, issue issueIdentityForOutput) (string, error) {
	ref, _, err := issueRefAndProjectForOutput(a, requestedID, requestedName, issue)
	return ref, err
}

// issueRefAndProjectForOutput also returns the current display project for
// clients which compose their own identity fields. A stable UID fallback
// leaves the project unknown rather than reporting the former project.
func issueRefAndProjectForOutput(a daemonAPI, requestedID int64, requestedName string, issue issueIdentityForOutput) (string, string, error) {
	if issue.ProjectID == 0 || issue.ProjectID == requestedID {
		return textsafe.Line(issue.ShortID), requestedName, nil
	}
	fallback := func(err error) (string, string, error) {
		ref, err := issueOutputFallback(issue, err)
		return ref, "", err
	}
	c, err := kataclient.NewWithHTTPClient(a.baseURL, a.client)
	if err != nil {
		return fallback(err)
	}
	response, callErr := c.ShowProjectWithResponse(a.ctx, &generated.ShowProjectRequestOptions{PathParams: &generated.ShowProjectPath{ProjectID: issue.ProjectID}})
	if response == nil {
		return fallback(externalCLITransportError(response, callErr))
	}
	if err := externalCLIResponseError(response.StatusCode, response.Body, callErr); err != nil {
		return fallback(err)
	}
	var body struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		return fallback(err)
	}
	if body.Project.Name == "" {
		return fallback(fmt.Errorf("project display lookup returned no name"))
	}
	if body.Project.Name == requestedName {
		return textsafe.Line(issue.ShortID), body.Project.Name, nil
	}
	return textsafe.Line(body.Project.Name + "#" + issue.ShortID), body.Project.Name, nil
}

func mutationIssueRefForOutput(cmd *cobra.Command, data []byte, requestedProject string) (string, error) {
	var body struct {
		Issue issueIdentityForOutput `json:"issue"`
		Event *struct {
			ProjectID   int64  `json:"project_id"`
			ProjectName string `json:"project_name"`
		} `json:"event"`
		Changed bool `json:"changed"`
		Reused  bool `json:"reused"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", err
	}
	if requestedProject == "" || body.Issue.ProjectID == 0 {
		return textsafe.Line(body.Issue.ShortID), nil
	}
	// Fresh issue events already carry the current canonical project name.
	// Replayed receipts can retain an earlier project name or ownership.
	if body.Changed && !body.Reused && body.Event != nil && body.Event.ProjectID == body.Issue.ProjectID && body.Event.ProjectName != "" {
		if body.Event.ProjectName == requestedProject {
			return textsafe.Line(body.Issue.ShortID), nil
		}
		return textsafe.Line(body.Event.ProjectName + "#" + body.Issue.ShortID), nil
	}
	a, err := dialDaemon(cmd.Context())
	if err != nil {
		return issueOutputFallback(body.Issue, err)
	}
	start, err := resolveStartPath(flags.Workspace)
	if err != nil {
		return issueOutputFallback(body.Issue, err)
	}
	pid, name, err := resolveProjectIDAndNameForRef(a, start, requestedProject, false)
	if err != nil {
		return issueOutputFallback(body.Issue, err)
	}
	return issueRefForOutput(a, pid, name, body.Issue)
}
