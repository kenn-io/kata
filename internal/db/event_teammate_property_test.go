package db_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"math"
	"strings"
	"testing"

	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/teammate"
	"hegel.dev/go/hegel"
)

// Property 10: stamping only appends attribution and preserves original values/bytes.
func TestPropertyStampPreservesPayload(t *testing.T) {
	encoded := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	leaf := hegel.OneOf(hegel.Map(hegel.Text(), func(s string) string { return encoded(s) }), hegel.Map(hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64)), func(n int64) string { return encoded(n) }), hegel.SampledFrom([]string{"null", "true", "false"}))
	value := hegel.Recursive(leaf, func(child hegel.Generator[string]) hegel.Generator[string] {
		return hegel.OneOf(hegel.Map(hegel.Lists(child), func(v []string) string { return "[" + strings.Join(v, ",") + "]" }), hegel.Map(hegel.Maps(hegel.Text(), child), func(v map[string]string) string {
			object := map[string]jsontext.Value{}
			for k, raw := range v {
				object[k] = jsontext.Value(raw)
			}
			return encoded(object)
		}))
	})
	hegel.Test(t, func(ht *hegel.T) {
		object := hegel.Draw(ht, hegel.Maps(hegel.Text(), value))
		handle, actor, kind := hegel.Draw(ht, hegel.Text()), hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"system"}))), hegel.Draw(ht, hegel.OneOf(hegel.Text(), hegel.SampledFrom([]string{"issue.created", "issue.commented"})))
		if hegel.Draw(ht, hegel.Booleans()) {
			object["teammate"] = hegel.Draw(ht, value)
		}
		raw := map[string]jsontext.Value{}
		for k, v := range object {
			raw[k] = jsontext.Value(v)
		}
		payload := " \n" + encoded(raw) + "\t"
		if hegel.Draw(ht, hegel.Booleans()) {
			payload = hegel.Draw(ht, value)
		}
		got, err := db.StampEventTeammate(db.WithEventTeammate(context.Background(), handle), kind, actor, payload)
		contextEligible := handle != "" && actor != "system" && kind != "issue.created" && kind != "issue.commented"
		if contextEligible && teammate.Validate(handle) != nil {
			if err == nil || got != payload {
				ht.Fatalf("invalid handle changed payload: %q => %q, %v", payload, got, err)
			}
			return
		}
		if err != nil || !jsontext.Value(got).IsValid() {
			ht.Fatalf("invalid output %q: %v", got, err)
		}
		twice, secondErr := db.StampEventTeammate(db.WithEventTeammate(context.Background(), handle), kind, actor, got)
		if secondErr != nil || twice != got {
			ht.Fatalf("second stamp changed payload: %q => %q, %v", got, twice, secondErr)
		}
		var before, after map[string]jsontext.Value
		isObject := jsontext.Value(payload).Kind() == '{'
		if isObject {
			if err := json.Unmarshal([]byte(payload), &before); err != nil {
				ht.Fatal(err)
			}
		}
		_, present := before["teammate"]
		if !isObject || present || handle == "" || actor == "system" || kind == "issue.created" || kind == "issue.commented" {
			if got != payload {
				ht.Fatalf("ineligible payload changed: %q => %q", payload, got)
			}
			return
		}
		if err := json.Unmarshal([]byte(got), &after); err != nil {
			ht.Fatal(err)
		}
		if len(after) != len(before)+1 || string(after["teammate"]) != encoded(handle) {
			ht.Fatalf("incorrect attribution: %s", got)
		}
		for key, want := range before {
			if string(after[key]) != string(want) {
				ht.Fatalf("changed %q: %s => %s", key, want, after[key])
			}
		}
		end := strings.LastIndex(payload, "}")
		if !strings.HasPrefix(got, payload[:end]) || !strings.HasSuffix(got, payload[end:]) {
			ht.Fatalf("original bytes changed: %q => %q", payload, got)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestPropertyStampBypassPreservesPayload(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mode := hegel.Draw(ht, hegel.SampledFrom([]string{"system", "created", "commented"}))
		handle := "/" + hegel.Draw(ht, hegel.Text())
		actor, kind := "operator", "future.event"
		switch mode {
		case "system":
			actor = "system"
		case "created":
			kind = "issue.created"
		case "commented":
			kind = "issue.commented"
		}
		payload := hegel.Draw(ht, hegel.Text())
		got, err := db.StampEventTeammate(db.WithEventTeammate(context.Background(), handle), kind, actor, payload)
		if err != nil || got != payload {
			ht.Fatalf("bypass changed payload %q => %q, %v", payload, got, err)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}

func TestPropertyStampNearMatchIsNotBypass(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mode := hegel.Draw(ht, hegel.SampledFrom([]string{"system", "created", "commented"}))
		suffix := hegel.Draw(ht, hegel.Text().MinSize(1))
		actor, kind := "operator", "future.event"
		switch mode {
		case "system":
			actor = "system" + suffix
		case "created":
			kind = "issue.created" + suffix
		case "commented":
			kind = "issue.commented" + suffix
		}
		got, err := db.StampEventTeammate(db.WithEventTeammate(context.Background(), "worker-1"), kind, actor, "{}")
		if err != nil || got != `{"teammate":"worker-1"}` {
			ht.Fatalf("near-match was bypassed: %q, %v", got, err)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
