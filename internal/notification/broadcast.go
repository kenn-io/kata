package notification

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// BroadcastRecipients resolves one bounded explicit audience. Teammate mode
// includes both the actor inbox and each exact teammate inbox.
func BroadcastRecipients(candidates []Identity, sender Identity, teammates bool) ([]string, error) {
	seen := map[string]bool{}
	self := Address(sender.Actor, sender.Teammate)
	for _, id := range candidates {
		if id.Actor == "" || id.Actor == "system" || id.Actor == "github-sync" || id.Actor == "notion-sync" || id.Actor == "plane-sync" || strings.HasPrefix(id.Actor, "integration:") {
			continue
		}
		addresses := []string{id.Actor}
		if teammates && id.Teammate != "" {
			addresses = append(addresses, Address(id.Actor, id.Teammate))
		}
		for _, r := range addresses {
			if r == self || (!teammates && r == sender.Actor) {
				continue
			}
			if _, e := NormalizeRecipient(r); e != nil {
				continue
			}
			seen[r] = true
		}
	}
	if len(seen) > 50 {
		return nil, errors.New("broadcast exceeds 50 recipients; use targeted --to requests")
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}

// BroadcastRecord represents one metadata event, regardless of its fan-out.
type BroadcastRecord struct {
	At              time.Time
	Sender, Message string
	Re              string
	Hidden          bool
}

// RateLimitError is returned without emitting any mutation event.
type RateLimitError struct {
	RetryAfterSeconds int       `json:"retry_after_seconds"`
	Window            string    `json:"window"`
	LastBroadcastAt   time.Time `json:"last_broadcast_at"`
	LastBroadcastBy   string    `json:"last_broadcast_by"`
	LastMessagePrefix string    `json:"last_message_prefix"`
}

func (e *RateLimitError) Error() string {
	return "broadcast rate limited; use targeted --to … --re instead"
}

// CheckBroadcastRate evaluates writing-daemon history, excluding boundary rows.
func CheckBroadcastRate(records []BroadcastRecord, sender, message string, now time.Time) *RateLimitError {
	var recent []BroadcastRecord
	var latest BroadcastRecord
	var retry time.Time
	window := ""
	for _, r := range records {
		if !r.At.After(now.Add(-time.Hour)) {
			continue
		}
		recent = append(recent, r)
		if latest.At.IsZero() || r.At.After(latest.At) {
			latest = r
		}
		if r.Sender != sender {
			continue
		}
		if r.Message == message {
			if end := r.At.Add(time.Hour); end.After(retry) {
				retry = end
				window = "1h"
			}
		}
		if r.At.After(now.Add(-10 * time.Minute)) {
			if end := r.At.Add(10 * time.Minute); end.After(retry) {
				retry = end
				window = "10m"
			}
		}
	}
	if len(recent) >= 3 {
		sort.Slice(recent, func(i, j int) bool { return recent[i].At.After(recent[j].At) })
		// The third-newest record must expire before an additional write is allowed.
		if end := recent[2].At.Add(time.Hour); end.After(retry) {
			retry = end
			window = "1h"
		}
	}
	if retry.IsZero() {
		return nil
	}
	prefix := latest.Message
	lastBroadcastAt := latest.At
	lastBroadcastBy := latest.Sender
	if latest.Hidden {
		prefix = ""
		lastBroadcastAt = time.Time{}
		lastBroadcastBy = ""
	}
	if len(prefix) > 128 {
		end := 128
		for end > 0 && !utf8.RuneStart(prefix[end]) {
			end--
		}
		prefix = prefix[:end]
	}
	return &RateLimitError{RetryAfterSeconds: max(1, int(math.Ceil(retry.Sub(now).Seconds()))), Window: window, LastBroadcastAt: lastBroadcastAt, LastBroadcastBy: lastBroadcastBy, LastMessagePrefix: prefix}
}

// BroadcastsFromPayload counts a single fan-out metadata event once. Only the
// new value is a broadcast: clearing an old marker does not consume a window.
func BroadcastsFromPayload(raw jsontext.Value, at time.Time) []BroadcastRecord {
	var payload struct {
		Diff map[string]struct {
			To jsontext.Value `json:"to"`
		} `json:"diff"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	keys := make([]string, 0, len(payload.Diff))
	for key := range payload.Diff {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !strings.HasPrefix(key, KeyPrefix) {
			continue
		}
		var value Value
		if json.Unmarshal(payload.Diff[key].To, &value) != nil || !value.Broadcast || value.From == "" {
			continue
		}
		return []BroadcastRecord{{At: at, Sender: Address(value.From, value.Teammate), Message: value.Message, Re: value.Re}}
	}
	return nil
}
