package notionsync

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const maxPropertyPages = 100
const maxPropertyValues = 10000

type wirePropertyItem struct {
	Object string `json:"object"`
	ID     string `json:"id"`
	Type   string `json:"type"`
	Title  *struct {
		PlainText *string `json:"plain_text"`
	} `json:"title"`
	People *struct {
		Object string `json:"object"`
		ID     string `json:"id"`
		Type   string `json:"type"`
	} `json:"people"`
}

type propertyPerson struct {
	id, object, kind string
}

type propertyValue struct {
	title   string
	people  []propertyPerson
	ownerID *string
}

func (v propertyValue) same(other propertyValue) bool {
	if v.title != other.title || len(v.people) != len(other.people) {
		return false
	}
	for i := range v.people {
		if v.people[i] != other.people[i] {
			return false
		}
	}
	return true
}

// propertyPath preserves Notion's returned percent encoding but prevents an
// opaque property ID from changing the origin, query, or API path structure.
func propertyPath(id string) (string, error) {
	decoded, err := url.PathUnescape(id)
	if err != nil || decoded == "" || decoded == "." || decoded == ".." {
		return "", fmt.Errorf("invalid Notion property ID")
	}
	if strings.ContainsAny(id, "/?#") {
		return url.PathEscape(decoded), nil
	}
	u := url.URL{Path: decoded, RawPath: id}
	return u.EscapedPath(), nil
}
func (s *clientSession) property(ctx context.Context, pageID, id, kind string) (propertyValue, error) {
	escaped, err := propertyPath(id)
	if err != nil {
		return propertyValue{}, err
	}
	base := "/v1/pages/" + pageID + "/properties/" + escaped
	params := url.Values{"page_size": []string{"100"}}
	cursors := map[string]bool{}
	values := 0
	var title strings.Builder
	var selected propertyValue
	for range maxPropertyPages {
		var wire struct {
			Object   string             `json:"object"`
			Type     string             `json:"type"`
			Results  []wirePropertyItem `json:"results"`
			HasMore  *bool              `json:"has_more"`
			Next     *string            `json:"next_cursor"`
			Property struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"property_item"`
		}
		if err := s.request(ctx, http.MethodGet, base+"?"+params.Encode(), nil, &wire); err != nil {
			return propertyValue{}, err
		}
		if wire.Object != "list" || wire.Type != "property_item" || wire.HasMore == nil || wire.Property.ID != id || wire.Property.Type != kind || wire.Results == nil {
			return propertyValue{}, fmt.Errorf("invalid Notion property response")
		}
		values += len(wire.Results)
		if values > maxPropertyValues {
			return propertyValue{}, fmt.Errorf("notion property exceeds 10000 values")
		}
		for _, item := range wire.Results {
			if item.Object != "property_item" || item.ID != id || item.Type != kind {
				return propertyValue{}, fmt.Errorf("invalid Notion property item identity")
			}
			switch kind {
			case "title":
				if item.Title == nil || item.Title.PlainText == nil {
					return propertyValue{}, fmt.Errorf("invalid Notion title item")
				}
				title.WriteString(*item.Title.PlainText)
			case "people":
				if item.People == nil {
					return propertyValue{}, fmt.Errorf("invalid Notion people item")
				}
				person := item.People
				personID, err := canonicalID(person.ID)
				if err != nil {
					return propertyValue{}, fmt.Errorf("invalid Notion people identity")
				}
				selected.people = append(selected.people, propertyPerson{id: personID, object: person.Object, kind: person.Type})
				if person.Object == "group" || person.Type == "group" {
					continue
				}
				if person.Object != "user" {
					return propertyValue{}, fmt.Errorf("invalid Notion people object")
				}
				if person.Type != "" && person.Type != "person" && person.Type != "bot" {
					return propertyValue{}, fmt.Errorf("invalid Notion user type")
				}
				if selected.ownerID == nil {
					selected.ownerID = &personID
				}
			}
		}
		cursor, err := nextNotionCursor(*wire.HasMore, wire.Next, cursors)
		if err != nil {
			return propertyValue{}, err
		}
		if !*wire.HasMore {
			selected.title = title.String()
			return selected, nil
		}
		params.Set("start_cursor", cursor)
	}
	return propertyValue{}, fmt.Errorf("notion property exceeds 100 response pages")
}
func (s *clientSession) markdown(ctx context.Context, id string) (string, error) {
	var wire struct {
		Object    string   `json:"object"`
		ID        string   `json:"id"`
		Markdown  *string  `json:"markdown"`
		Truncated *bool    `json:"truncated"`
		Unknown   []string `json:"unknown_block_ids"`
	}
	if err := s.request(ctx, http.MethodGet, "/v1/pages/"+id+"/markdown", nil, &wire); err != nil {
		return "", err
	}
	if _, err := validateResponseID(wire.Object, "page_markdown", wire.ID, id); err != nil {
		return "", err
	}
	if wire.Markdown == nil || wire.Truncated == nil || wire.Unknown == nil {
		return "", fmt.Errorf("invalid Notion markdown response")
	}
	if *wire.Truncated || len(wire.Unknown) > 0 {
		return "", fmt.Errorf("notion markdown is truncated or contains unknown block IDs")
	}
	if len(*wire.Markdown) > maxMarkdownBytes {
		return "", fmt.Errorf("notion markdown exceeds 1 MiB")
	}
	return *wire.Markdown, nil
}
func (s *clientSession) Content(ctx context.Context, cfg Config, page Page) (PageContent, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return PageContent{}, err
	}
	page.ID, err = canonicalID(page.ID)
	if err != nil {
		return PageContent{}, err
	}
	if page.IsArchived || page.InTrash || page.DataSourceID != cfg.DataSourceID || page.DatabaseID != cfg.DatabaseID {
		return PageContent{}, fmt.Errorf("notion page is unavailable or moved")
	}
	for range 2 {
		title, err := s.property(ctx, page.ID, cfg.TitlePropertyID, "title")
		if err != nil {
			return PageContent{}, err
		}
		people, err := s.property(ctx, page.ID, cfg.AssigneePropertyID, "people")
		if err != nil {
			return PageContent{}, err
		}
		markdown, err := s.markdown(ctx, page.ID)
		if err != nil {
			return PageContent{}, err
		}
		var wire wirePage
		if err := s.request(ctx, http.MethodGet, "/v1/pages/"+page.ID, nil, &wire); err != nil {
			return PageContent{}, err
		}
		if _, err := validateResponseID(wire.Object, "page", wire.ID, page.ID); err != nil {
			return PageContent{}, err
		}
		fresh, available, err := wire.observation(cfg)
		if err != nil {
			return PageContent{}, err
		}
		if !available {
			return PageContent{}, fmt.Errorf("notion page became unavailable or moved during content read")
		}
		verifiedTitle, err := s.property(ctx, page.ID, cfg.TitlePropertyID, "title")
		if err != nil {
			return PageContent{}, err
		}
		verifiedPeople, err := s.property(ctx, page.ID, cfg.AssigneePropertyID, "people")
		if err != nil {
			return PageContent{}, err
		}
		var verifiedWire wirePage
		if err := s.request(ctx, http.MethodGet, "/v1/pages/"+page.ID, nil, &verifiedWire); err != nil {
			return PageContent{}, err
		}
		if _, err := validateResponseID(verifiedWire.Object, "page", verifiedWire.ID, page.ID); err != nil {
			return PageContent{}, err
		}
		verifiedPage, available, err := verifiedWire.observation(cfg)
		if err != nil {
			return PageContent{}, err
		}
		if !available {
			return PageContent{}, fmt.Errorf("notion page became unavailable or moved during content read")
		}
		if samePage(page, fresh) && title.same(verifiedTitle) && people.same(verifiedPeople) && samePage(fresh, verifiedPage) {
			return PageContent{Page: verifiedPage, Title: title.title, Markdown: markdown, OwnerID: people.ownerID}, nil
		}
		page = verifiedPage
	}
	return PageContent{}, fmt.Errorf("notion page changed during both content reads")
}
