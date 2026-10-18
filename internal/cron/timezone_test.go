package cron

import (
	"archive/zip"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestTriggerRejectsLocalTimezone(t *testing.T) {
	for _, trigger := range []Trigger{
		{Kind: "manual"}, {Kind: "cron", Cron: "0 9 * * *"},
		{Kind: "interval", IntervalSeconds: 60}, {Kind: "once", At: "2026-01-15T09:00:00Z"},
		{Kind: "issue-scheduled", IssueUID: definitionUID}, {Kind: "issue-deadline", IssueUID: definitionUID},
	} {
		t.Run(trigger.Kind, func(t *testing.T) {
			trigger.Timezone = "Local"
			err := trigger.Validate()
			require.ErrorIs(t, err, ErrInvalid)
			require.ErrorContains(t, err, `timezone "Local" depends on the hub's host`)
			require.ErrorContains(t, err, "UTC")
		})
	}
}

func TestTriggerRejectsNonPortableTimezones(t *testing.T) {
	for _, zone := range []string{"EST", "MST", "HST", "EST5EDT", "PST8PDT", "CET", "GMT", "Etc/GMT+5", "Etc/GMT", "Zulu", "UCT", "US/Eastern", "Japan", "Factory"} {
		t.Run(zone, func(t *testing.T) {
			err := (Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: zone}).Validate()
			require.ErrorIs(t, err, ErrInvalid)
			require.ErrorContains(t, err, fmt.Sprintf("timezone %q is not a portable IANA name", zone))
			for _, suggestion := range map[string][]string{
				"US/Eastern": {"America/New_York"}, "EST": {"America/Panama", "America/New_York"},
				"GMT": {"use UTC"}, "Japan": {"use an Area/Location name"},
			}[zone] {
				require.ErrorContains(t, err, suggestion)
			}
		})
	}
}

func TestTriggerRejectsNamesOutsideBundle(t *testing.T) {
	for _, zone := range []string{"africa/Abidjan", "america/new_york", "america/New_York", "America/New_york", "America/NEW_YORK", "posixrules", "Not/AZone"} {
		for _, directive := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/directive=%v", zone, directive), func(t *testing.T) {
				trigger := Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: zone}
				if directive {
					trigger.Timezone = ""
					trigger.Cron = "CRON_TZ=" + zone + " 0 9 * * *"
				}
				require.EqualError(t, trigger.Validate(), "invalid cron document: invalid timezone")
			})
		}
	}
}

func TestTriggerAcceptsPortableTimezones(t *testing.T) {
	for _, zone := range []string{"", "UTC", "Etc/UTC", "America/New_York", "America/Argentina/Buenos_Aires", "America/Port-au-Prince", "Antarctica/DumontDUrville", "Asia/Calcutta", "Europe/Kiev"} {
		t.Run(zone, func(t *testing.T) {
			require.NoError(t, (Trigger{Kind: "cron", Cron: "0 9 * * *", Timezone: zone}).Validate())
			if zone != "" {
				require.NoError(t, (Trigger{Kind: "cron", Cron: "TZ=" + zone + " 0 9 * * *"}).Validate())
			}
		})
	}
}

func TestTriggerRejectsDirectiveZoneDeterministically(t *testing.T) {
	job, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	job.Trigger = Trigger{Kind: "cron", Cron: "TZ=US/Eastern 0 9 * * *"}
	err = job.Validate()
	require.ErrorIs(t, err, ErrInvalid)
	require.ErrorContains(t, err, "use America/New_York")
}

// Names and their case are portable across host filesystems. Exercise the
// public field, both directive prefixes, and their adapter job entry point.
func TestTriggerTimezoneCaseProperty(t *testing.T) {
	zones := toolchainZoneNames(t)
	var geographic []string
	for _, zone := range zones {
		if geographicTimezonePattern.MatchString(zone) {
			geographic = append(geographic, zone)
		}
	}
	job, err := ParseJob([]byte(executeJSON))
	require.NoError(t, err)
	rapid.Check(t, func(t *rapid.T) {
		original := rapid.SampledFrom(geographic).Draw(t, "geographic zone")
		// Lowercase the area, and vary the remaining uppercase letters too.
		zone := []byte(original)
		for i, c := range zone {
			if c >= 'A' && c <= 'Z' && (i == 0 || rapid.Bool().Draw(t, "lowercase letter")) {
				zone[i] = c + ('a' - 'A')
			}
		}
		name := string(zone)
		for _, trigger := range []Trigger{
			{Kind: "cron", Cron: "0 9 * * *", Timezone: name},
			{Kind: "cron", Cron: "TZ=" + name + " 0 9 * * *"},
			{Kind: "cron", Cron: "CRON_TZ=" + name + " 0 9 * * *"},
		} {
			require.EqualError(t, trigger.Validate(), "invalid cron document: invalid timezone")
			current := job
			current.Trigger = trigger
			require.EqualError(t, current.Validate(), "invalid cron document: invalid timezone")
		}
	})
}

var geographicTimezonePattern = regexp.MustCompile(`^(Africa|America|Antarctica|Arctic|Asia|Atlantic|Australia|Europe|Indian|Pacific)/`)

func TestTriggerTimezoneNameProperty(t *testing.T) {
	names := toolchainZoneNames(t)
	allowed := map[string]bool{"": true, "UTC": true, "Etc/UTC": true}
	for _, name := range names {
		if geographicTimezonePattern.MatchString(name) {
			allowed[name] = true
		}
	}
	rapid.Check(t, func(t *rapid.T) {
		var name string
		if rapid.Bool().Draw(t, "bundled name") {
			name = rapid.SampledFrom(names).Draw(t, "zone")
		} else {
			name = rapid.String().Draw(t, "zone")
		}
		err := (Trigger{Kind: "manual", Timezone: name}).Validate()
		if allowed[name] {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrInvalid)
		}
	})
}

func toolchainZoneNames(t *testing.T) []string {
	t.Helper()
	archive, err := zip.OpenReader(toolchainZoneArchivePath(t))
	require.NoError(t, err)
	defer func() { _ = archive.Close() }()
	names := make([]string, 0, len(archive.File))
	for _, file := range archive.File {
		names = append(names, file.Name)
	}
	return names
}

func TestTimezoneReplacementsArePortable(t *testing.T) {
	targetPattern := regexp.MustCompile(`[A-Z][A-Za-z]+/[A-Za-z0-9_+/-]+|UTC`)
	for name, suggestion := range timezoneReplacements {
		t.Run(name, func(t *testing.T) {
			_, exists := bundledZones[name]
			require.True(t, exists, "suggestion key must exist in bundled tzdata")
			require.ErrorIs(t, portableTimezone(name), ErrInvalid)
			targets := targetPattern.FindAllString(suggestion, -1)
			require.NotEmpty(t, targets, "suggestions must include a portable target")
			for _, target := range targets {
				require.NoError(t, portableTimezone(target), "suggested target %s", target)
			}
		})
	}
}

func TestBundledZoneIndexMatchesToolchain(t *testing.T) {
	archive, err := zip.OpenReader(toolchainZoneArchivePath(t))
	require.NoError(t, err)
	defer func() { _ = archive.Close() }()
	var expected, actual []string
	for _, file := range archive.File {
		expected = append(expected, file.Name)
	}
	for name := range bundledZones {
		actual = append(actual, name)
	}
	slices.Sort(expected)
	slices.Sort(actual)
	require.Equal(t, expected, actual, "run go generate ./internal/cron")
}

func toolchainZoneArchivePath(t *testing.T) string {
	t.Helper()
	goroot, err := exec.Command("go", "env", "GOROOT").Output() //nolint:gosec // Fixed Go command locates the toolchain used by this test.
	require.NoError(t, err)
	return filepath.Join(strings.TrimSpace(string(goroot)), "lib", "time", "zoneinfo.zip")
}
