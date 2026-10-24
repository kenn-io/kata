package tui

import (
	"context"
	"errors"
	"testing"

	cronui "go.kenn.io/kata/internal/tui/cron"
	"go.kenn.io/kata/internal/tui/cron/records"
)

type cronCapabilityRetryAPI struct {
	cronTestAPI
	instanceCalls int
}

func (a *cronCapabilityRetryAPI) GetInstance(context.Context) (InstanceInfo, error) {
	a.instanceCalls++
	if a.instanceCalls == 1 {
		return InstanceInfo{}, errors.New("temporary connection failure")
	}
	return InstanceInfo{Auth: AuthInfo{Kind: "local"}}, nil
}

func (*cronCapabilityRetryAPI) ListCronJobs(context.Context, int64) ([]records.Job, error) {
	return []records.Job{{UID: "recovered-job", Name: "Recovered job"}}, nil
}

func TestCronRefreshRecoversInitialCapabilityFailure(t *testing.T) {
	api := &cronCapabilityRetryAPI{}
	n := initialModel(Options{})
	n.api = api
	n.scope.projectID = 7
	n.scope.projectName = "example-project"
	n.authCapabilitiesRequired = true
	n.authCapabilitiesReady = false
	h := newCronHost(n)
	h.tab = 1
	h.Update(h.native.fetchAuthCapabilities()())
	if h.native.authCapabilitiesReady || h.native.activeAuthError == "" {
		t.Fatal("initial permissions failure did not leave capabilities unavailable")
	}
	_, cmd := h.Update(cronui.RefreshMsg{})
	if cmd == nil {
		t.Fatal("cron refresh cannot recover unavailable permissions")
	}
	drainCronSmoke(t, h, cmd)
	if !h.native.authCapabilitiesReady || h.native.activeAuthError != "" {
		t.Fatal("cron refresh did not restore permissions")
	}
	jobs := h.catalogs[7].jobs
	if len(jobs) != 1 || jobs[0].UID != "recovered-job" {
		t.Fatalf("cron refresh did not load recovered catalog: %+v", jobs)
	}
}
