package db_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kata/internal/db"
)

func TestIssue_HasShortIDFieldAndNoNumber(t *testing.T) {
	typ := reflect.TypeFor[db.Issue]()
	_, hasShortID := typ.FieldByName("ShortID")
	_, hasNumber := typ.FieldByName("Number")
	assert.True(t, hasShortID, "Issue.ShortID should exist")
	assert.False(t, hasNumber, "Issue.Number should be removed")
}
