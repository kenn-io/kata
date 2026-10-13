package forgejo

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/kata/pkg/connector"
)

func descriptors() []connector.FieldDescriptor {
	return []connector.FieldDescriptor{
		{ID: "due_date", DisplayName: "Due date", AcceptedKinds: []string{"date"}, Nullable: true, Writable: true, SchemaRevision: "forgejo-v1"},
		{ID: "labels", DisplayName: "Labels", AcceptedKinds: []string{"string_set"}, Writable: true, SchemaRevision: "forgejo-v1"},
		{ID: "assignees", DisplayName: "Assignees", AcceptedKinds: []string{"string_set"}, Writable: true, SchemaRevision: "forgejo-v1"},
		{ID: "milestone", DisplayName: "Milestone", AcceptedKinds: []string{"string"}, Nullable: true, Writable: true, SchemaRevision: "forgejo-v1"},
	}
}
func knownField(id string) bool {
	return id == "due_date" || id == "labels" || id == "assignees" || id == "milestone"
}
func fieldValues(i issue) (map[string]connector.FieldValue, *connector.Error) {
	values := map[string]connector.FieldValue{"due_date": {Kind: "null"}, "milestone": {Kind: "null"}, "labels": {Kind: "string_set", Values: []string{}}, "assignees": {Kind: "string_set", Values: []string{}}}
	if i.DueDate != nil {
		values["due_date"] = connector.FieldValue{Kind: "date", Value: i.DueDate.Format("2006-01-02")}
	}
	if i.Milestone != nil {
		if i.Milestone.ID <= 0 {
			return nil, failure("provider_error", "Forgejo milestone identity is invalid")
		}
		values["milestone"] = connector.FieldValue{Kind: "string", Value: id(i.Milestone.ID)}
	}
	labels := []string{}
	for _, label := range i.Labels {
		if label.ID <= 0 {
			return nil, failure("provider_error", "Forgejo label identity is invalid")
		}
		labels = append(labels, id(label.ID))
	}
	values["labels"] = connector.FieldValue{Kind: "string_set", Values: labels}
	assignees := []string{}
	for _, assignee := range i.Assignees {
		if assignee.Login == "" {
			return nil, failure("provider_error", "Forgejo assignee identity is invalid")
		}
		assignees = append(assignees, assignee.Login)
	}
	values["assignees"] = connector.FieldValue{Kind: "string_set", Values: assignees}
	for key, value := range values {
		canonical, err := connector.CanonicalizeFieldValue(value)
		if err != nil {
			return nil, failure("provider_error", "Forgejo returned an invalid field")
		}
		values[key] = canonical
	}
	return values, nil
}
func selectFields(i issue, ids []string) (map[string]connector.FieldValue, *connector.Error) {
	all, err := fieldValues(i)
	if err != nil {
		return nil, err
	}
	selected := map[string]connector.FieldValue{}
	for _, id := range ids {
		if !knownField(id) {
			return nil, failure("unsupported", "Forgejo field is unsupported")
		}
		selected[id] = all[id]
	}
	return selected, nil
}
func (h *handler) ListFields(ctx context.Context, _ connector.ListFieldsParams) (connector.ListFieldsResult, *connector.Error) {
	c, err := h.client(ctx)
	if err != nil {
		return connector.ListFieldsResult{}, err
	}
	if _, err := c.self(ctx); err != nil {
		return connector.ListFieldsResult{}, err
	}
	return connector.ListFieldsResult{Fields: descriptors()}, nil
}
func (h *handler) ReadFields(ctx context.Context, p connector.ReadFieldsParams) (connector.ReadFieldsResult, *connector.Error) {
	for _, field := range p.FieldIDs {
		if !knownField(field) {
			return connector.ReadFieldsResult{}, failure("unsupported", "Forgejo field is unsupported")
		}
	}
	c, err := h.client(ctx)
	if err != nil {
		return connector.ReadFieldsResult{}, err
	}
	i, _, err := c.load(ctx, p.RootKey)
	if err != nil {
		return connector.ReadFieldsResult{}, err
	}
	values, err := selectFields(i, p.FieldIDs)
	return connector.ReadFieldsResult{Fields: values}, err
}
func validateField(id string, value connector.FieldValue) *connector.Error {
	if !knownField(id) {
		return failure("unsupported", "Forgejo field is unsupported")
	}
	if connector.ValidateFieldValue(value) != nil {
		return failure("invalid_fields", "Forgejo field value is invalid")
	}
	valid := false
	switch id {
	case "due_date":
		valid = value.Kind == "date" || value.Kind == "null"
	case "labels":
		valid = value.Kind == "string_set"
		for _, label := range value.Values {
			if _, ok := numeric(label); !ok {
				valid = false
			}
		}
	case "assignees":
		valid = value.Kind == "string_set"
		for _, login := range value.Values {
			if login == "" || strings.TrimSpace(login) != login {
				valid = false
			}
		}
	case "milestone":
		valid = value.Kind == "null"
		if value.Kind == "string" {
			_, valid = numeric(value.Value)
		}
	}
	if !valid {
		return failure("invalid_fields", "Forgejo field value is unsupported or invalid")
	}
	return nil
}
func (h *handler) WriteFields(ctx context.Context, p connector.WriteFieldsParams) (connector.WriteFieldsResult, *connector.Error) {
	// Stock Forgejo has no atomic field precondition. A read followed by PATCH
	// would lie about CAS; reject every expected map before provider writes.
	if p.Expected != nil {
		return connector.WriteFieldsResult{}, unsupported()
	}
	for key, value := range p.Fields {
		if err := validateField(key, value); err != nil {
			return connector.WriteFieldsResult{}, err
		}
	}
	c, err := h.client(ctx)
	if err != nil {
		return connector.WriteFieldsResult{}, err
	}
	current, path, err := c.load(ctx, p.RootKey)
	if err != nil {
		return connector.WriteFieldsResult{}, err
	}
	repoPath := strings.TrimSuffix(path, "/issues/"+id(current.Number))
	patch := map[string]any{}
	ids := []string{}
	var labels []int64
	// Validate every catalog before touching either mutation endpoint.
	for key, value := range p.Fields {
		ids = append(ids, key)
		switch key {
		case "due_date":
			if value.Kind == "null" {
				patch["unset_due_date"] = true
			} else {
				date, _ := time.Parse("2006-01-02", value.Value)
				patch["due_date"] = date.Format(time.RFC3339)
			}
		case "labels":
			labels = []int64{}
			if len(value.Values) > 0 {
				catalog, err := pages[label](ctx, c, repoPath+"/labels")
				if err != nil {
					return connector.WriteFieldsResult{}, err
				}
				available := map[int64]bool{}
				for _, v := range catalog {
					available[v.ID] = true
				}
				missing := false
				for _, s := range value.Values {
					n, _ := numeric(s)
					missing = missing || !available[n]
				}
				if missing {
					// Repository catalogs omit labels owned by the repository's
					// organization. Discover only the current owner namespace,
					// derived from the immutable repository lookup above.
					owner, _, _ := strings.Cut(strings.TrimPrefix(repoPath, "/repos/"), "/")
					var organization struct {
						ID       int64  `json:"id"`
						Username string `json:"username"`
					}
					err := c.request(ctx, "GET", "/orgs/"+owner, nil, &organization)
					if err != nil && err.Code != "not_found" {
						return connector.WriteFieldsResult{}, err
					}
					if err == nil {
						if organization.ID <= 0 || organization.Username != owner {
							return connector.WriteFieldsResult{}, failure("provider_error", "Forgejo repository organization is invalid")
						}
						organizationLabels, err := pages[label](ctx, c, "/orgs/"+owner+"/labels")
						if err != nil {
							return connector.WriteFieldsResult{}, err
						}
						for _, v := range organizationLabels {
							available[v.ID] = true
						}
					}
				}
				for _, s := range value.Values {
					n, _ := numeric(s)
					if !available[n] {
						return connector.WriteFieldsResult{}, failure("invalid_fields", "Forgejo label is unknown")
					}
					labels = append(labels, n)
				}
			}
		case "assignees":
			selected := []string{}
			if len(value.Values) > 0 {
				var catalog []user
				err := c.request(ctx, "GET", repoPath+"/assignees", nil, &catalog)
				if err != nil {
					return connector.WriteFieldsResult{}, err
				}
				available := map[string]bool{}
				for _, v := range catalog {
					available[v.Login] = true
				}
				for _, login := range value.Values {
					if !available[login] {
						return connector.WriteFieldsResult{}, failure("invalid_fields", "Forgejo assignee is unknown")
					}
					selected = append(selected, login)
				}
			}
			patch["assignees"] = selected
		case "milestone":
			n := int64(0)
			if value.Kind == "string" {
				n, _ = numeric(value.Value)
				catalog, err := pages[label](ctx, c, repoPath+"/milestones?state=all")
				if err != nil {
					return connector.WriteFieldsResult{}, err
				}
				found := false
				for _, v := range catalog {
					found = found || v.ID == n
				}
				if !found {
					return connector.WriteFieldsResult{}, failure("invalid_fields", "Forgejo milestone is unknown")
				}
			}
			patch["milestone"] = n
		}
	}
	if labels != nil {
		if err := c.request(ctx, "PUT", path+"/labels", map[string]any{"labels": labels}, nil); err != nil {
			return connector.WriteFieldsResult{}, err
		}
	}
	if len(patch) > 0 {
		if err := c.request(ctx, "PATCH", path, patch, nil); err != nil {
			return connector.WriteFieldsResult{}, err
		}
	}
	fresh, _, err := c.load(ctx, p.RootKey)
	if err != nil {
		return connector.WriteFieldsResult{}, err
	}
	values, err := selectFields(fresh, ids)
	if err != nil {
		return connector.WriteFieldsResult{}, err
	}
	if wanted, ok := p.Fields["due_date"]; ok && wanted.Kind == "date" && fresh.DueDate != nil &&
		values["due_date"].Kind == "date" {
		requestedDate, _ := time.Parse("2006-01-02", wanted.Value)
		_, offset := fresh.DueDate.Zone()
		if offset > 0 && values["due_date"].Value == requestedDate.AddDate(0, 0, 1).Format("2006-01-02") {
			// Forgejo stores the submitted calendar date at the end of that day
			// in the submitted offset, then reads it in its configured UI zone.
			// Reissue the same date using the returned provider offset so a
			// positive UI offset does not turn the readback into the next day.
			correctedDate := time.Date(requestedDate.Year(), requestedDate.Month(), requestedDate.Day(), 0, 0, 0, 0, fresh.DueDate.Location())
			if err := c.request(ctx, "PATCH", path, map[string]any{"due_date": correctedDate.Format(time.RFC3339)}, nil); err != nil {
				return connector.WriteFieldsResult{}, err
			}
			fresh, _, err = c.load(ctx, p.RootKey)
			if err != nil {
				return connector.WriteFieldsResult{}, err
			}
			values, err = selectFields(fresh, ids)
			if err != nil {
				return connector.WriteFieldsResult{}, err
			}
		}
	}
	for key, want := range p.Fields {
		if digest(values[key]) != digest(want) {
			return connector.WriteFieldsResult{}, failure("provider_error", "Forgejo field readback did not match; read fresh fields before retrying")
		}
	}
	return connector.WriteFieldsResult{Fields: values}, nil
}
