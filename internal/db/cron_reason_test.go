package db

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"go.kenn.io/kata/internal/cron"
	"hegel.dev/go/hegel"
)

// The SQL predicate must agree with the shared reader across arbitrary reason
// codes and message text, while preserving job/project scope and page order.
func TestCronRunReasonSQLProperty(t *testing.T) {
	tx, _, _ := referenceWorkFixture(t, 0)
	ctx := t.Context()
	sqlCron := CronSQL{Query: tx}
	hegel.Test(t, func(ht *hegel.T) {
		code := hegel.Draw(ht, hegel.Text().Alphabet("abcdefghijklmnopqrstuvwxyz0123456789_.-").MinSize(1).MaxSize(64))
		other := "other"
		if code == other {
			other = "different"
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM cron_runs"); err != nil {
			ht.Fatal(err)
		}
		count := hegel.Draw(ht, hegel.Integers[int](1, 20))
		job := "01M40000000000000000000001"
		filterJob := hegel.Draw(ht, hegel.Booleans())
		want := []string{}
		for i := range count {
			message := hegel.Draw(ht, hegel.Text().MaxSize(40))
			s := cron.Summary{Version: 1, Message: message}
			switch hegel.Draw(ht, hegel.Integers[int](0, 7)) {
			case 0:
				s.Reason = code
			case 1:
				s.Message = "reason: " + code
			case 2:
				s.Message = "reason: " + code + "\n" + message
			case 3:
				s.Message = "reason: " + code + "\r\n" + message
			case 4:
				s.Message = "reason: " + code + " " + message
			case 5:
				s.Message = message + "\nreason: " + code
			case 6:
				s.Reason = other
				s.Message = "reason: " + code
			}
			project := hegel.Draw(ht, hegel.Integers[int](1, 2))
			rowJob := job
			if hegel.Draw(ht, hegel.Booleans()) {
				rowJob = "01M40000000000000000000002"
			}
			id := fmt.Sprintf("%026d", i+1)
			raw, err := json.Marshal(s)
			if err != nil {
				ht.Fatal(err)
			}
			at := time.Date(2026, 11, 3, 0, 0, 0, i, time.UTC).Format(CronRunCreatedAtFormat)
			_, err = tx.ExecContext(ctx, `INSERT INTO cron_runs(uid,project_id,job_uid,actor,status,summary_json,revision,created_at,updated_at) VALUES($1,$2,$3,'operator','failed',$4,1,$5,$5)`, id, project, rowJob, string(raw), at)
			if err != nil {
				ht.Fatal(err)
			}
			reason, ok := cron.RunReason(s)
			if project == 1 && (!filterJob || rowJob == job) && ok && reason == code {
				want = append(want, id)
			}
		}
		slices.Reverse(want)
		in := CronRunList{ProjectID: 1, Reason: code, Limit: hegel.Draw(ht, hegel.Integers[int](1, 10))}
		if filterJob {
			in.JobUID = job
		}
		got := []string{}
		for range count + 1 {
			rows, err := sqlCron.Runs(ctx, in)
			if err != nil {
				ht.Fatal(err)
			}
			if len(rows) == 0 {
				break
			}
			for _, row := range rows {
				got = append(got, row.UID)
			}
			in.BeforeUID = rows[len(rows)-1].UID
		}
		if !slices.Equal(want, got) {
			ht.Fatalf("code=%q want=%v got=%v", code, want, got)
		}
		for _, bad := range []string{strings.ToUpper("INVALID"), code + "\n"} {
			if _, err := sqlCron.Runs(ctx, CronRunList{ProjectID: 1, Reason: bad}); err == nil {
				ht.Fatalf("accepted invalid reason %q", bad)
			}
		}
	}, hegel.WithTestCases(2000), hegel.WithDatabase(t.TempDir()))
}
