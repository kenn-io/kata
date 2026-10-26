package db_test

import (
	"context"
	"testing"

	"go.kenn.io/kata/internal/db"
)

func TestStampEventTeammate(t *testing.T) {
	ctx := db.WithEventTeammate(context.Background(), "worker-1")
	for _, tt := range []struct {
		name, kind, actor, payload, want string
		wantErr                          bool
	}{
		{"append preserves bytes", "issue.closed", "operator", " \n{ \"z\":9007199254740993, \"a\": [1e+20,\"\\u0061\"] }\t", " \n{ \"z\":9007199254740993, \"a\": [1e+20,\"\\u0061\"] ,\"teammate\":\"worker-1\"}\t", false},
		{"empty object", "future.event", "operator", "{ }", "{ \"teammate\":\"worker-1\"}", false},
		{"existing string", "issue.closed", "operator", `{"teammate":"original"}`, `{"teammate":"original"}`, false},
		{"existing null", "issue.closed", "operator", `{"teammate":null}`, `{"teammate":null}`, false},
		{"existing number escaped key", "issue.closed", "operator", `{"team\u006date":7}`, `{"team\u006date":7}`, false},
		{"system bypasses parsing", "issue.closed", "system", "{", "{", false},
		{"created", "issue.created", "operator", "{", "{", false},
		{"commented", "issue.commented", "operator", "{", "{", false},
		{"run observed preserves missing record teammate", "cron.run.observed", "operator", `{"actor":"worker"}`, `{"actor":"worker"}`, false},
		{"run snapshot preserves missing record teammate", "cron.run.snapshot", "operator", `{"actor":"worker"}`, `{"actor":"worker"}`, false},
		{"nonobject", "issue.closed", "operator", " [1,2] ", " [1,2] ", false},
		{"malformed", "issue.closed", "operator", "{", "{", true},
		{"trailing value", "issue.closed", "operator", "{} {}", "{} {}", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.StampEventTeammate(ctx, tt.kind, tt.actor, tt.payload)
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("got %q, %v; want %q, error=%v", got, err, tt.want, tt.wantErr)
			}
		})
	}
	cleared := db.WithEventTeammate(ctx, "")
	if db.EventTeammateFromContext(ctx) != "worker-1" || db.EventTeammateFromContext(cleared) != "" {
		t.Fatal("context override changed parent or retained inherited teammate")
	}
	if got, err := db.StampEventTeammate(cleared, "issue.closed", "operator", "{"); got != "{" || err != nil {
		t.Fatalf("empty override: %q, %v", got, err)
	}
	t.Run("property shrunk empty object", func(t *testing.T) {
		got, err := db.StampEventTeammate(db.WithEventTeammate(context.Background(), "0"), "", "", " \n{}\t")
		if got != " \n{\"teammate\":\"0\"}\t" || err != nil {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("invalid handle is rejected before stamping", func(t *testing.T) {
		const payload = `{"state":"closed"}`
		got, err := db.StampEventTeammate(db.WithEventTeammate(context.Background(), "bad/handle"), "issue.closed", "operator", payload)
		if got != payload || err == nil {
			t.Fatalf("got %q, %v; want unchanged payload and validation error", got, err)
		}
	})
}

func TestEventTeammateDerivation(t *testing.T) {
	for _, tt := range []struct{ kind, payload, want string }{
		{"issue.closed", `{"teammate":"worker-1"}`, "worker-1"},
		{"issue.created", `{"metadata":{"teammate":"worker-2"}}`, "worker-2"},
		{"issue.closed", `{"metadata":{"teammate":"worker-2"}}`, ""},
		{"issue.created", `{"teammate":"worker-1","metadata":{"teammate":"worker-2"}}`, "worker-1"},
		{"issue.created", `{"teammate":null,"metadata":{"teammate":"worker-2"}}`, ""},
		{"issue.created", `{"teammate":"","metadata":{"teammate":"worker-2"}}`, ""},
		{"issue.closed", `{"teammate":7}`, ""},
		{"issue.closed", `{"teammate":"bad/handle"}`, ""},
		{"issue.created", `{"metadata":{"teammate":"bad/handle"}}`, ""},
		{"issue.closed", `[]`, ""},
		{"issue.closed", `{`, ""},
	} {
		if got := db.EventTeammate(tt.kind, tt.payload); got != tt.want {
			t.Errorf("EventTeammate(%q, %s) = %q, want %q", tt.kind, tt.payload, got, tt.want)
		}
	}
}
