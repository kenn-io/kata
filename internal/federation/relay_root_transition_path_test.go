package federation

import (
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/db"
)

// R5: validate the entire signed replacement before any native pin mutation.
func TestRelayRootTransitionPath(t *testing.T) {
	project, authority := "00000000000000000000000001", "00000000000000000000000002"
	key := func() (db.RootKeyPin, ed25519.PrivateKey) {
		public, private, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		return db.RootKeyPin{ProjectUID: project, AuthorityUID: authority, KeyID: db.RootPublicKeyID(public), PublicKey: public}, private
	}
	first, firstPrivate := key()
	second, secondPrivate := key()
	third, _ := key()
	a, err := db.SignRootKeyTransition(first, second, firstPrivate)
	require.NoError(t, err)
	b, err := db.SignRootKeyTransition(second, third, secondPrivate)
	require.NoError(t, err)
	path, err := relayRootTransitionPath(first, third, []db.RootKeyTransition{b, a})
	require.NoError(t, err)
	require.Equal(t, []db.RootKeyTransition{a, b}, path, "history order is not authority")
	path, err = relayRootTransitionPath(second, third, []db.RootKeyTransition{b, a})
	require.NoError(t, err)
	require.Equal(t, []db.RootKeyTransition{b}, path, "resume from a durably retained intermediate pin")
	for _, name := range []string{"missing", "bad-first", "bad-last", "branch", "foreign-project", "foreign-root", "retired", "wrong-advertised-key", "cycle"} {
		t.Run(name, func(t *testing.T) {
			history := []db.RootKeyTransition{a, b}
			advertised := third
			switch name {
			case "missing":
				history = history[:1]
			case "bad-first", "bad-last":
				index := 0
				if name == "bad-last" {
					index = 1
				}
				history[index].Signature = append([]byte(nil), history[index].Signature...)
				history[index].Signature[0] ^= 1
			case "branch":
				history = append(history, a)
			case "foreign-project":
				advertised.ProjectUID = "00000000000000000000000003"
			case "foreign-root":
				advertised.AuthorityUID = "00000000000000000000000003"
			case "retired":
				advertised.Retired = true
			case "wrong-advertised-key":
				advertised.PublicKey = first.PublicKey
			case "cycle":
				history[1], err = db.SignRootKeyTransition(second, first, secondPrivate)
				require.NoError(t, err)
			}
			path, err := relayRootTransitionPath(first, advertised, history)
			require.Error(t, err)
			require.Nil(t, path, "invalid tail cannot return a partially authorized path")
		})
	}
}
