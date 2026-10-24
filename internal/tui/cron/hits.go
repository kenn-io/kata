package cron

type tabHit struct{ Index, X0, X1 int }
type hits struct {
	Rows                               map[int]rowHit
	Tabs                               []tabHit
	TabY, Top, Scroll, BodyRows, Width int
}

func (h hits) At(x, y int) (rowHit, bool) {
	if x < 0 || x >= h.Width || y < 0 {
		return rowHit{}, false
	}
	hit, ok := h.Rows[y]
	return hit, ok && x >= hit.X0 && x < hit.X1
}
func (h hits) TabAt(x, y int) (int, bool) {
	if x < 0 || x >= h.Width || y != h.TabY {
		return 0, false
	}
	for _, hit := range h.Tabs {
		if x >= hit.X0 && x < hit.X1 {
			return hit.Index, true
		}
	}
	return 0, false
}
