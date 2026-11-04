package cron

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Contract: compare fields without rounding opaque numbers, and produce
// sequentially applicable JSON Pointer changes with absent sides omitted.
func TestDiffDocumentsFieldChanges(t *testing.T) {
	changes, err := DiffDocuments(jsontext.Value(`{"a/b~c":[null,2,3],"n":9007199254740993,"gone":true}`), jsontext.Value(`{"a/b~c":[false],"n":9007199254740994,"added":null}`))
	require.NoError(t, err)
	raw, err := json.Marshal(changes)
	require.NoError(t, err)
	require.JSONEq(t, `[
 {"op":"replace","path":"/a~1b~0c/0","before":null,"after":false},
 {"op":"remove","path":"/a~1b~0c/2","before":3},
 {"op":"remove","path":"/a~1b~0c/1","before":2},
 {"op":"add","path":"/added","after":null},
 {"op":"remove","path":"/gone","before":true},
 {"op":"replace","path":"/n","before":9007199254740993,"after":9007199254740994}]`, string(raw))
	require.Contains(t, string(raw), `9007199254740993`)
	require.Contains(t, string(raw), `9007199254740994`)
	changes, err = DiffDocuments(jsontext.Value(`{"z":1, "text":"\u0061"}`), jsontext.Value(`{"text":"a","z":1}`))
	require.NoError(t, err)
	require.NotNil(t, changes)
	require.Empty(t, changes)
	changes, err = DiffDocuments(jsontext.Value(`1.000000000000000000001`), jsontext.Value(`1e0`))
	require.NoError(t, err)
	require.Equal(t, "", changes[0].Path)
	require.Equal(t, `1.000000000000000000001`, string(changes[0].Before))
}

func TestDiffDocumentsRejectInvalid(t *testing.T) {
	for _, bad := range []string{"", "{", `{"x":1,"x":2}`, "true false", "[1,]"} {
		_, err := DiffDocuments(jsontext.Value(bad), jsontext.Value(`null`))
		require.Error(t, err, bad)
		_, err = DiffDocuments(jsontext.Value(`null`), jsontext.Value(bad))
		require.Error(t, err, bad)
	}
}

func drawDiffDocument(ht *hegel.T, depth int) jsontext.Value {
	kind := hegel.Draw(ht, hegel.Integers(0, 6))
	if depth >= 4 {
		kind %= 5
	}
	switch kind {
	case 0:
		return jsontext.Value(`null`)
	case 1:
		return jsontext.Value(strconv.FormatBool(hegel.Draw(ht, hegel.Booleans())))
	case 2:
		return jsontext.Value(strconv.FormatInt(hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64))), 10))
	case 3:
		return jsontext.Value(fmt.Sprintf("%d.%018d", hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64))), hegel.Draw(ht, hegel.Integers(int64(0), int64(math.MaxInt64)))))
	case 4:
		text := []rune(hegel.Draw(ht, hegel.Text()))
		if len(text) > 64 {
			text = text[:64]
		}
		raw, err := json.Marshal(string(text))
		require.NoError(ht, err)
		return raw
	case 5:
		items := hegel.Draw(ht, hegel.Lists(hegel.Integers(0, math.MaxInt)))
		if len(items) > 6 {
			items = items[:6]
		}
		values := make([]jsontext.Value, len(items))
		for i := range values {
			values[i] = drawDiffDocument(ht, depth+1)
		}
		raw, err := json.Marshal(values)
		require.NoError(ht, err)
		return raw
	default:
		keys := hegel.Draw(ht, hegel.Lists(hegel.Text()))
		if len(keys) > 6 {
			keys = keys[:6]
		}
		values := map[string]jsontext.Value{}
		for _, key := range keys {
			values[key] = drawDiffDocument(ht, depth+1)
		}
		raw, err := json.Marshal(values)
		require.NoError(ht, err)
		return raw
	}
}

func TestDiffDocumentsIdentityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := drawDiffDocument(ht, 0)
		changes, err := DiffDocuments(value, value)
		require.NoError(ht, err)
		require.NotNil(ht, changes)
		require.Empty(ht, changes)
	}, hegel.WithTestCases(1000))
}

func TestDiffDocumentsApplyProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		before, after := drawDiffDocument(ht, 0), drawDiffDocument(ht, 0)
		changes, err := DiffDocuments(before, after)
		require.NoError(ht, err)
		result := before
		for _, change := range changes {
			parts := []string{}
			if change.Path != "" {
				require.True(ht, strings.HasPrefix(change.Path, "/"))
				for part := range strings.SplitSeq(change.Path[1:], "/") {
					parts = append(parts, strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~"))
				}
			}
			result = applyDiffChange(ht, result, parts, change)
		}
		require.Equal(ht, canonicalDiffValue(ht, after), canonicalDiffValue(ht, result))
	}, hegel.WithTestCases(1000))
}

func canonicalDiffValue(t require.TestingT, raw jsontext.Value) string {
	var object map[string]jsontext.Value
	if raw.Kind() == '{' {
		require.NoError(t, json.Unmarshal(raw, &object))
		for k, v := range object {
			object[k] = jsontext.Value(canonicalDiffValue(t, v))
		}
		out, err := json.Marshal(object, json.Deterministic(true))
		require.NoError(t, err)
		return string(out)
	}
	if raw.Kind() == '[' {
		var array []jsontext.Value
		require.NoError(t, json.Unmarshal(raw, &array))
		for i, v := range array {
			array[i] = jsontext.Value(canonicalDiffValue(t, v))
		}
		out, err := json.Marshal(array)
		require.NoError(t, err)
		return string(out)
	}
	if raw.Kind() == '"' {
		var str string
		require.NoError(t, json.Unmarshal(raw, &str))
		out, err := json.Marshal(str)
		require.NoError(t, err)
		return string(out)
	}
	out := raw.Clone()
	require.NoError(t, out.Compact())
	return string(out)
}

func applyDiffChange(t require.TestingT, raw jsontext.Value, path []string, change Change) jsontext.Value {
	if len(path) == 0 {
		require.Equal(t, string(raw), string(change.Before))
		return change.After
	}
	if raw.Kind() == '{' {
		var obj map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(raw, &obj))
		if len(path) == 1 {
			old, exists := obj[path[0]]
			if change.Op == "add" {
				require.False(t, exists)
			} else {
				require.True(t, exists)
				require.Equal(t, canonicalDiffValue(t, old), canonicalDiffValue(t, change.Before))
			}
			if change.Op == "remove" {
				delete(obj, path[0])
			} else {
				obj[path[0]] = change.After
			}
		} else {
			obj[path[0]] = applyDiffChange(t, obj[path[0]], path[1:], change)
		}
		out, err := json.Marshal(obj)
		require.NoError(t, err)
		return out
	}
	var array []jsontext.Value
	require.NoError(t, json.Unmarshal(raw, &array))
	index, err := strconv.Atoi(path[0])
	require.NoError(t, err)
	if len(path) > 1 {
		array[index] = applyDiffChange(t, array[index], path[1:], change)
	} else {
		if change.Op != "add" {
			require.Equal(t, canonicalDiffValue(t, array[index]), canonicalDiffValue(t, change.Before))
		}
		switch change.Op {
		case "add":
			require.Equal(t, len(array), index)
			array = append(array, change.After)
		case "remove":
			array = append(array[:index], array[index+1:]...)
		default:
			array[index] = change.After
		}
	}
	out, err := json.Marshal(array)
	require.NoError(t, err)
	return out
}

func TestDiffDocumentsNumericTokensProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		integer := strconv.FormatInt(hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64))), 10)
		exponent := strconv.FormatInt(hegel.Draw(ht, hegel.Integers(int64(math.MinInt64), int64(math.MaxInt64))), 10)
		before, after := jsontext.Value(integer), jsontext.Value(integer+"e"+exponent)
		changes, err := DiffDocuments(before, after)
		require.NoError(ht, err)
		require.Len(ht, changes, 1)
		require.Equal(ht, integer, string(changes[0].Before))
		require.Equal(ht, string(after), string(changes[0].After))
	}, hegel.WithTestCases(1000))
}
