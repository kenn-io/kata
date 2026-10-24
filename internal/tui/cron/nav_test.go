package cron

import "testing"

func TestDetailItemKeyRoundTrip(t *testing.T) {
	for _, kind := range []string{"run", "step", "used-by", "job"} {
		for _, project := range []string{"7", "8"} {
			uid := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
			if kind == "step" {
				uid = "summarize_结果"
			}
			key := detailItemKey(kind, project, uid)
			k, p, u, ok := parseDetailItemKey(key)
			if !ok || k != kind || p != project || u != uid {
				t.Fatal(key, k, p, u, ok)
			}
		}
	}
	if detailItemKey("run", "7", "01ARZ3NDEKTSV4RRFFQ69G5FAV") == detailItemKey("run", "8", "01ARZ3NDEKTSV4RRFFQ69G5FAV") {
		t.Fatal("collision")
	}
}
