package cron

import (
	"slices"

	tea "charm.land/bubbletea/v2"
)

type workflowWrapKey struct {
	workflow    workflowKey
	event, step string
	width       int
}

func (m *model) workflowSelection(v *workflowView) (kind, uid string, ok bool) {
	kind, project, uid, ok := parseDetailItemKey(m.detailKey)
	if !ok || v == nil || project != v.key.projectUID {
		return "", "", false
	}
	switch kind {
	case "step":
		ok = slices.Contains(v.check.Order, uid)
	case "used-by":
		ok = false
		for _, j := range v.jobs {
			if m.data.Jobs[j.index].UID == uid {
				ok = true
				break
			}
		}
	case "run":
		ok = false
		for _, i := range v.runs {
			r := m.data.Runs[i]
			if r.UID == uid && r.ProjectKey == project {
				ok = true
				break
			}
		}
	default:
		ok = false
	}
	return kind, uid, ok
}
func (m *model) workflowDetailKey(k tea.KeyPressMsg) tea.Cmd {
	kind, uid, ok := m.workflowSelection(m.selectedWorkflow())
	if matchesKey(k.String(), keys.Toggle, keys.Open, keys.Right, keys.Accept) && ok && kind == "step" {
		m.expanded[m.detailKey] = !m.expanded[m.detailKey]
		m.follow = true
		return nil
	}
	_ = uid
	return m.routeScreenKey(k)
}
