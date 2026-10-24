package cron

import (
	"slices"
	"strings"

	"go.kenn.io/kata/internal/tui/cron/records"
)

// Resolve recorded native issue identities within the captured project.
// Rendering never reads the daemon. Observations have no ShortID, so keep
// their full UID instead of inventing a suffix that may collide.
func (m *model) knownIssueRef(projectUID, uid string) string {
	return m.issueRefIndex().known(projectUID, uid)
}

// IssueRefs holds every saved ref by project and issue UID. The first ref
// wins: jobs' saved references, then runs in order.
type issueRefs struct {
	refs  map[[2]string]string
	names map[string]string // Project names by UID, looked up once.
}

// IssueRefIndex resolves refs for a whole list in one pass over the data.
func (m *model) issueRefIndex() issueRefs {
	idx := issueRefs{refs: map[[2]string]string{}, names: map[string]string{}}
	for _, r := range m.data.Runs {
		if uid := records.Value(r.IssueUID); uid != "" {
			idx.refs[[2]string{r.ProjectKey, uid}] = qualifiedIssueRef(m.projectName(r.ProjectKey), uid)
		}
	}
	return idx
}
func (idx issueRefs) project(m *model, uid string) string {
	name, ok := idx.names[uid]
	if !ok {
		name = m.projectName(uid)
		idx.names[uid] = name
	}
	return name
}

func (idx issueRefs) known(projectUID, uid string) string {
	if strings.Contains(uid, "#") {
		return uid
	}
	if ref := idx.refs[[2]string{projectUID, uid}]; ref != "" {
		return ref
	}
	return uid
}

// RunIssueRef is the run's own issue as run detail shows it.
func (m *model) runIssueRef(r records.Run) string {
	return qualifiedIssueRef(m.projectName(r.ProjectKey), records.Value(r.IssueUID))
}

func (idx issueRefs) run(m *model, r records.Run) string {
	return qualifiedIssueRef(idx.project(m, r.ProjectKey), records.Value(r.IssueUID))
}

// ShortIssueRef matches Kata's # column: an issue in project drops its
// prefix. Other projects keep their qualified ref.
func shortIssueRef(project, ref string) string {
	if ref == "" {
		return "—"
	}
	if canonicalUID(ref) {
		return ref
	}
	i := strings.LastIndex(ref, "#")
	if i < 0 {
		return "#" + ref
	}
	if i == 0 || ref[:i] == project {
		return ref[i:]
	}
	return ref
}

// DefaultProject is the project whose issues a list shows as a bare #abcd:
// the lane's own project, else the first configured project, else the first
// lane by name.
func (m *model) defaultProject() string {
	if m.opts.Project != "" {
		return m.opts.Project
	}
	var names []string
	for _, p := range m.data.Projects {
		names = append(names, p.Project)
	}
	slices.Sort(names)
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// ListIssueRef fills a list's # cell. Lists have no project column, so an
// issue outside the default project keeps its project#abcd ref; with a single
// project every ref is a bare #abcd.
func (m *model) listIssueRef(project, ref string) string {
	if ref == "" {
		return "—"
	}
	if canonicalUID(ref) {
		return ref
	}
	if !m.showProjects() {
		if i := strings.LastIndex(ref, "#"); i >= 0 {
			return ref[i:]
		}
		return "#" + ref
	}
	return shortIssueRef(m.defaultProject(), qualifiedIssueRef(project, ref))
}

// ListName prefixes a row's name with its project, as Kata's all-projects
// list prefixes titles, when the row is outside the default project and has
// no issue ref to say so.
func (m *model) listName(project, name, issue string) string {
	if strings.HasPrefix(issue, project+"#") || !m.showProjects() || project == "" || project == m.defaultProject() {
		return name
	}
	return "[" + project + "] " + name
}

func qualifiedIssueRef(project, ref string) string {
	if strings.Contains(ref, "#") {
		return ref
	}
	if canonicalUID(ref) {
		return ref
	}
	if ref != "" && project != "" {
		return project + "#" + ref
	}
	return ref
}
