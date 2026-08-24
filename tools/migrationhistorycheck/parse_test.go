package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseNameStatusAssignsPathRolesPerStatus(t *testing.T) {
	tests := map[string]struct {
		diff string
		want []stagedChange
	}{
		"add and modify use destination": {
			diff: "A\tinternal/db/pgstore/migrations/000007_add_thing.up.sql\nM\tinternal/db/pgstore/migrations/000008_edit_thing.up.sql",
			want: []stagedChange{
				{status: 'A', dst: "internal/db/pgstore/migrations/000007_add_thing.up.sql"},
				{status: 'M', dst: "internal/db/pgstore/migrations/000008_edit_thing.up.sql"},
			},
		},
		"delete uses source": {
			diff: "D\tinternal/db/pgstore/migrations/000007_remove_thing.up.sql",
			want: []stagedChange{{status: 'D', src: "internal/db/pgstore/migrations/000007_remove_thing.up.sql"}},
		},
		"rename and copy preserve both filtered paths": {
			diff: "R100\tinternal/db/pgstore/migrations/000007_old.up.sql\tinternal/db/pgstore/migrations/000008_new.up.sql\nC100\tinternal/db/pgstore/migrations/000009_source.up.sql\tinternal/db/pgstore/migrations/000010_copy.up.sql",
			want: []stagedChange{
				{status: 'R', src: "internal/db/pgstore/migrations/000007_old.up.sql", dst: "internal/db/pgstore/migrations/000008_new.up.sql"},
				{status: 'C', src: "internal/db/pgstore/migrations/000009_source.up.sql", dst: "internal/db/pgstore/migrations/000010_copy.up.sql"},
			},
		},
		"filters paths and malformed records": {
			diff: "\nM\tinternal/db/pgstore/migrations/readme.txt\nM\tother/000007_outside.up.sql\nR100\tinternal/db/pgstore/migrations/000007_old.txt\tinternal/db/pgstore/migrations/000008_new.up.sql\nC100\tinternal/db/pgstore/migrations/000009_source.up.sql\tother/000010_copy.up.sql\nM\tinternal/db/pgstore/migrations/000011_valid.up.sql\nmalformed",
			want: []stagedChange{
				{status: 'R', dst: "internal/db/pgstore/migrations/000008_new.up.sql"},
				{status: 'C', src: "internal/db/pgstore/migrations/000009_source.up.sql"},
				{status: 'M', dst: "internal/db/pgstore/migrations/000011_valid.up.sql"},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseNameStatus(tt.diff, "internal/db/pgstore/migrations"))
		})
	}
}

func TestStagedChangeBaseRefPathPicksThePathThatCanExistOnBase(t *testing.T) {
	tests := []struct {
		name   string
		change stagedChange
		want   string
	}{
		{name: "add", change: stagedChange{status: 'A', dst: "new.sql"}, want: "new.sql"},
		{name: "modify", change: stagedChange{status: 'M', dst: "changed.sql"}, want: "changed.sql"},
		{name: "delete", change: stagedChange{status: 'D', src: "deleted.sql"}, want: "deleted.sql"},
		{name: "rename", change: stagedChange{status: 'R', src: "old.sql", dst: "new.sql"}, want: "old.sql"},
		{name: "copy", change: stagedChange{status: 'C', src: "source.sql", dst: "copy.sql"}, want: "copy.sql"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.change.baseRefPath())
		})
	}
}
