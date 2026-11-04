package main

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestCronBundleDefinitionEqualityPreservesNumbersProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
		left := jsontext.Value(fmt.Sprintf(`{"a":%d,"b":[1,2]}`, n))
		reordered := jsontext.Value(fmt.Sprintf("{ \"b\": [1, 2], \"a\": %d }", n))
		equal, err := cronBundleDefinitionEqual(left, reordered)
		require.NoError(ht, err)
		require.True(ht, equal)
		changed := jsontext.Value(fmt.Sprintf(`{"a":%d,"b":[1,2]}`, n+1))
		equal, err = cronBundleDefinitionEqual(left, changed)
		require.NoError(ht, err)
		require.False(ht, equal)
	}, hegel.WithTestCases(100))
}
func TestCronBundleDefinitionEqualityTokenSpellingAndArrayOrder(t *testing.T) {
	for _, pair := range [][2]string{{`{"n":1}`, `{"n":1.0}`}, {`{"n":1e0}`, `{"n":1E+0}`}, {`{"n":9007199254740992}`, `{"n":9007199254740993}`}, {`{"a":[1,2]}`, `{"a":[2,1]}`}} {
		equal, err := cronBundleDefinitionEqual([]byte(pair[0]), []byte(pair[1]))
		require.NoError(t, err)
		require.False(t, equal)
	}
}

func drawBundleComparison(ht *hegel.T) (jsontext.Value, jsontext.Value, jsontext.Value) {
	n := hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0)))
	token := fmt.Sprintf("%d", n)
	if hegel.Draw(ht, hegel.Booleans()) {
		token = "-" + token
	}
	if hegel.Draw(ht, hegel.Booleans()) {
		token += fmt.Sprintf(".%020d", hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0))))
	}
	if hegel.Draw(ht, hegel.Booleans()) {
		token += hegel.Draw(ht, hegel.SampledFrom([]string{"e+", "e-", "E+", "E-"})) + fmt.Sprintf("%020d", hegel.Draw(ht, hegel.Integers[uint64](0, ^uint64(0))))
	}
	changed := token + "0"
	if token == "0" || token == "-0" {
		changed = token + ".0"
	}
	left := jsontext.Value(`{"outer":[{"n":` + token + `,"s":"\u00e9"}],"flag":true}`)
	right := jsontext.Value(`{ "flag": true, "outer": [ { "s": "é", "n": ` + token + ` } ] }`)
	different := jsontext.Value(`{"outer":[{"n":` + changed + `,"s":"é"}],"flag":true}`)
	return left, right, different
}
func TestCronBundleNestedNumericNormalizationProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		left, right, _ := drawBundleComparison(ht)
		equal, err := cronBundleDefinitionEqual(left, right)
		require.NoError(ht, err)
		require.True(ht, equal)
	}, hegel.WithTestCases(100))
}
func TestCronBundleNestedNumericDistinctionProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		left, _, different := drawBundleComparison(ht)
		equal, err := cronBundleDefinitionEqual(left, different)
		require.NoError(ht, err)
		require.False(ht, equal)
	}, hegel.WithTestCases(100))
}
