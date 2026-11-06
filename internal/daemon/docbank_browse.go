package daemon

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
	"uuid"

	"go.kenn.io/kata/internal/api"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/docbank"
)

const maxDocbankBrowseBytes = 1 << 20
const maxDocbankBrowsePathBytes = 4096

func canonicalDocbankBrowsePath(value string) bool {
	return value != "" && len(value) <= maxDocbankBrowsePathBytes && utf8.ValidString(value) &&
		strings.HasPrefix(value, "/") && path.Clean(value) == value && !strings.ContainsAny(value, "\\\x00")
}

func validDocbankBrowseNode(node api.DocbankBrowseNode) bool {
	if node.ID <= 0 || node.Size < 0 || node.Kind != "dir" && node.Kind != "file" || !canonicalDocbankBrowsePath(node.Path) {
		return false
	}
	if node.CurrentVersionID != nil && *node.CurrentVersionID == (uuid.UUID{}) {
		return false
	}
	if node.Path == "/" {
		return node.Kind == "dir" && node.Name == "/"
	}
	return node.Name == path.Base(node.Path) && validDocbankBrowseName(node.Name)
}

func validDocbankBrowseName(name string) bool {
	// Native names have no attachment-name length/control-character restriction.
	// Backslash remains unsupported by the existing exposed-path policy.
	return name != "" && name != "." && name != ".." && utf8.ValidString(name) && !strings.ContainsAny(name, "/\\\x00")
}

func projectDocbankBrowseNode(node docbank.Node) (api.DocbankBrowseNode, error) {
	out := api.DocbankBrowseNode{ID: node.ID, Name: node.Name, Kind: node.Kind, Size: node.Size, Path: node.Path}
	if out.Path == "/" && out.Name == "" {
		// Native root names may be empty; the public catalog renders root as /.
		out.Name = "/"
	}
	if node.CurrentVersionID != (uuid.UUID{}) {
		out.CurrentVersionID = new(node.CurrentVersionID)
	}
	if node.TrashedAt != "" || !validDocbankBrowseNode(out) {
		return out, attachmentError(502, "docbank_scope_unavailable")
	}
	return out, nil
}

func docbankBrowseRoute(ctx context.Context, cfg ServerConfig, project db.Project, expected string) (api.DocbankServiceCapabilities, *db.FederationBinding, error) {
	caps, err := docbankOriginCapabilities(ctx, cfg, project)
	if err != nil {
		return caps, nil, err
	}
	if !caps.Browse {
		return caps, nil, attachmentError(503, "docbank_forward_unsupported")
	}
	if expected != "" && caps.Origin != expected {
		return caps, nil, attachmentError(409, "attachment_origin_mismatch")
	}
	binding, err := docbankUpstream(ctx, cfg, project.ID)
	if err == nil && (binding == nil) != (caps.Origin == cfg.DB.InstanceUID()) {
		err = attachmentError(409, "attachment_origin_mismatch")
	}
	return caps, binding, err
}

func finishDocbankBrowse(ctx context.Context, cfg ServerConfig, project db.Project, selected *db.FederationBinding, origin string, result any) error {
	current, err := docbankUpstream(ctx, cfg, project.ID)
	if err != nil {
		return err
	}
	if (current == nil) != (selected == nil) {
		return attachmentError(409, "attachment_origin_mismatch")
	}
	if current != nil && (current.HubURL != selected.HubURL || current.HubProjectID != selected.HubProjectID || current.HubProjectUID != selected.HubProjectUID) {
		return attachmentError(409, "attachment_origin_mismatch")
	}
	// A fresh preflight also rechecks stored credential pinning, outgoing access
	// and the recursively selected origin. Do not retry against a changed route.
	caps, err := docbankOriginCapabilities(ctx, cfg, project)
	if err != nil {
		return err
	}
	if !caps.Browse || caps.Origin != origin {
		return attachmentError(409, "attachment_origin_mismatch")
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxDocbankBrowseBytes {
		return attachmentError(502, "docbank_browse_invalid")
	}
	return nil
}

func decodeDocbankBrowseResponse(resp *http.Response, result any) error {
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDocbankBrowseBytes+1))
	if err != nil || len(raw) > maxDocbankBrowseBytes {
		return attachmentError(502, "docbank_browse_invalid")
	}
	if _, page := result.(*api.DocbankChildrenBody); page {
		var fields map[string]jsontext.Value
		if json.Unmarshal(raw, &fields) != nil {
			return attachmentError(502, "docbank_browse_invalid")
		}
		for _, name := range []string{"directory", "items", "total", "limit", "offset"} {
			value := bytes.TrimSpace(fields[name])
			if len(value) == 0 || bytes.Equal(value, []byte("null")) {
				return attachmentError(502, "docbank_browse_invalid")
			}
		}
	}
	if json.Unmarshal(raw, result) != nil {
		return attachmentError(502, "docbank_browse_invalid")
	}
	return nil
}

func resolveOriginDocbank(ctx context.Context, cfg ServerConfig, project db.Project, requested, expected string) (api.DocbankResolveBody, error) {
	var out api.DocbankResolveBody
	if !canonicalDocbankBrowsePath(requested) {
		return out, attachmentError(400, "docbank_path_invalid")
	}
	caps, binding, err := docbankBrowseRoute(ctx, cfg, project, expected)
	if err != nil {
		return out, err
	}
	if binding != nil {
		query := url.Values{"project_uid": {project.UID}, "origin": {caps.Origin}, "path": {requested}}
		resp, err := docbankForwardRequest(ctx, cfg, *binding, http.MethodGet, "resolve", query, nil)
		if err != nil {
			return out, err
		}
		if err := decodeDocbankBrowseResponse(resp, &out); err != nil {
			return out, err
		}
	} else {
		if err := exposedDocbankNode(cfg, docbank.Node{Path: requested}); err != nil {
			return out, err
		}
		node, err := cfg.Docbank.ResolvePath(ctx, requested)
		if err != nil {
			return out, docbankError(err, docbankLinkErrors)
		}
		if err := exposedDocbankNode(cfg, node); err != nil {
			return out, err
		}
		out.Node, err = projectDocbankBrowseNode(node)
		if err != nil {
			return out, err
		}
		out.ProjectUID, out.Origin = project.UID, caps.Origin
	}
	if out.ProjectUID != project.UID || out.Origin != caps.Origin || out.Node.Path != requested || !validDocbankBrowseNode(out.Node) {
		return out, attachmentError(502, "docbank_browse_invalid")
	}
	return out, finishDocbankBrowse(ctx, cfg, project, binding, caps.Origin, out)
}

func validDocbankBrowsePage(page api.DocbankChildrenBody, requested string, limit, offset int) bool {
	if !validDocbankBrowseNode(page.Directory) || page.Directory.Kind != "dir" || requested != "" && page.Directory.Path != requested ||
		page.Limit != limit || page.Offset != offset || page.Total < 0 || len(page.Items) > limit || page.Items == nil {
		return false
	}
	// Subtraction avoids overflowing offset+len(items) at the full int boundary.
	remaining := 0
	if offset < page.Total {
		remaining = page.Total - offset
	}
	if len(page.Items) != min(limit, remaining) {
		return false
	}
	seen := make(map[int64]bool, len(page.Items))
	for _, child := range page.Items {
		if !validDocbankBrowseName(child.Name) || !validDocbankBrowseNode(child) || child.Path != path.Join(page.Directory.Path, child.Name) || child.ID == page.Directory.ID || seen[child.ID] {
			return false
		}
		seen[child.ID] = true
	}
	return true
}

func listOriginDocbank(ctx context.Context, cfg ServerConfig, project db.Project, params api.DocbankBrowseParams, expected string) (api.DocbankChildrenBody, error) {
	var out api.DocbankChildrenBody
	if params.Path != "" && !canonicalDocbankBrowsePath(params.Path) {
		return out, attachmentError(400, "docbank_path_invalid")
	}
	if params.Limit < 1 || params.Limit > 200 || params.Offset < 0 {
		return out, attachmentError(400, "docbank_page_invalid")
	}
	caps, binding, err := docbankBrowseRoute(ctx, cfg, project, expected)
	if err != nil {
		return out, err
	}
	if binding != nil {
		query := url.Values{"project_uid": {project.UID}, "origin": {caps.Origin}, "path": {params.Path}, "limit": {strconv.Itoa(params.Limit)}, "offset": {strconv.Itoa(params.Offset)}}
		resp, err := docbankForwardRequest(ctx, cfg, *binding, http.MethodGet, "children", query, nil)
		if err != nil {
			return out, err
		}
		if err := decodeDocbankBrowseResponse(resp, &out); err != nil {
			return out, err
		}
	} else {
		requested := params.Path
		if requested == "" {
			requested = path.Clean("/" + strings.Trim(cfg.DocbankFolder, "/"))
		}
		if !canonicalDocbankBrowsePath(requested) {
			return out, attachmentError(502, "docbank_scope_unavailable")
		}
		if err := exposedDocbankNode(cfg, docbank.Node{Path: requested}); err != nil {
			return out, err
		}
		selected, err := cfg.Docbank.ResolvePath(ctx, requested)
		if err != nil {
			return out, docbankError(err, docbankLinkErrors)
		}
		if selected.ID <= 0 || selected.Kind != "dir" || selected.Path != requested {
			return out, attachmentError(502, "docbank_scope_unavailable")
		}
		page, err := cfg.Docbank.Children(ctx, selected.ID, params.Limit, params.Offset)
		if err != nil {
			return out, docbankError(err, docbankLinkErrors)
		}
		if page.Directory.ID != selected.ID || page.Directory.Path != requested {
			return out, attachmentError(502, "docbank_scope_unavailable")
		}
		if err := exposedDocbankNode(cfg, page.Directory); err != nil {
			return out, err
		}
		out.Directory, err = projectDocbankBrowseNode(page.Directory)
		if err != nil {
			return out, err
		}
		if len(page.Items) > params.Limit {
			return out, attachmentError(502, "docbank_browse_invalid")
		}
		out.Items = make([]api.DocbankBrowseNode, 0, len(page.Items))
		for _, child := range page.Items {
			if !validDocbankBrowseName(child.Name) {
				return out, attachmentError(502, "docbank_browse_invalid")
			}
			child.Path = path.Join(out.Directory.Path, child.Name)
			projected, err := projectDocbankBrowseNode(child)
			if err != nil {
				return out, err
			}
			out.Items = append(out.Items, projected)
		}
		out.ProjectUID, out.Origin = project.UID, caps.Origin
		out.Total, out.Limit, out.Offset = page.Total, page.Limit, page.Offset
	}
	if out.ProjectUID != project.UID || out.Origin != caps.Origin || !validDocbankBrowsePage(out, params.Path, params.Limit, params.Offset) {
		return out, attachmentError(502, "docbank_browse_invalid")
	}
	return out, finishDocbankBrowse(ctx, cfg, project, binding, caps.Origin, out)
}
