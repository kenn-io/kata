package cronbundle

import (
	"encoding/json/jsontext"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestCronBundleDefinitionEqualityPreservesNumbersProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		n := rapid.Uint64().Draw(ht, "integer")
		left := jsontext.Value(fmt.Sprintf(`{"a":%d,"b":[1,2]}`, n))
		reordered := jsontext.Value(fmt.Sprintf("{ \"b\": [1, 2], \"a\": %d }", n))
		equal, err := DefinitionEqual(left, reordered)
		require.NoError(ht, err)
		require.True(ht, equal)
		changed := jsontext.Value(fmt.Sprintf(`{"a":%d,"b":[1,2]}`, n+1))
		equal, err = DefinitionEqual(left, changed)
		require.NoError(ht, err)
		require.False(ht, equal)
	})
}
func TestCronBundleDefinitionEqualityTokenSpellingAndArrayOrder(t *testing.T) {
	for _, pair := range [][2]string{{`{"n":1}`, `{"n":1.0}`}, {`{"n":1e0}`, `{"n":1E+0}`}, {`{"n":9007199254740992}`, `{"n":9007199254740993}`}, {`{"a":[1,2]}`, `{"a":[2,1]}`}} {
		equal, err := DefinitionEqual([]byte(pair[0]), []byte(pair[1]))
		require.NoError(t, err)
		require.False(t, equal)
	}
}

func drawBundleComparison(ht *rapid.T) (jsontext.Value, jsontext.Value, jsontext.Value) {
	n := rapid.Uint64().Draw(ht, "integer")
	token := fmt.Sprintf("%d", n)
	if rapid.Bool().Draw(ht, "choice") {
		token = "-" + token
	}
	if rapid.Bool().Draw(ht, "choice") {
		token += fmt.Sprintf(".%020d", rapid.Uint64().Draw(ht, "integer"))
	}
	if rapid.Bool().Draw(ht, "choice") {
		token += rapid.SampledFrom([]string{"e+", "e-", "E+", "E-"}).Draw(ht, "exponent") + fmt.Sprintf("%020d", rapid.Uint64().Draw(ht, "integer"))
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
	rapid.Check(t, func(ht *rapid.T) {
		left, right, _ := drawBundleComparison(ht)
		equal, err := DefinitionEqual(left, right)
		require.NoError(ht, err)
		require.True(ht, equal)
	})
}
func TestCronBundleNestedNumericDistinctionProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		left, _, different := drawBundleComparison(ht)
		equal, err := DefinitionEqual(left, different)
		require.NoError(ht, err)
		require.False(ht, equal)
	})
}
