package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kata/internal/federationsigning"
)

// Contract: normal daemon startup opts into a separate restricted listener,
// advertises its private ephemeral address and releases replay ownership on stop.
func TestDaemonStartsConfiguredFederationIngress(t *testing.T) {
	for _, secret := range []string{strings.Repeat("k", 64), ""} {
		t.Run(fmt.Sprintf("key available %t", secret != ""), func(t *testing.T) {
			resetFlags(t)
			home := setupKataEnv(t)
			t.Setenv("PORT", "")
			t.Setenv("TEST_DAEMON_SIGNING_KEY", secret)
			require.NoError(t, federationsigning.InitializeReplayState(filepath.Join(home, "federation-signing-replay.state")))
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte(`[federation.signing]
required = true
external_url = "https://daemon.example/federation"
[[federation.signing.key]]
key_id = "key-a"
enrollment_id = 1
key_env = "TEST_DAEMON_SIGNING_KEY"
[federation.ingress]
enabled = true
`), 0600))
			read, write, err := os.Pipe()
			require.NoError(t, err)
			previous := os.Stderr
			os.Stderr = write
			t.Cleanup(func() { os.Stderr = previous; _ = write.Close(); _ = read.Close() })
			addresses := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(read)
				for scanner.Scan() {
					if address, ok := strings.CutPrefix(scanner.Text(), "federation ingress listening at "); ok {
						addresses <- address
						return
					}
				}
			}()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			exited := make(chan struct{})
			go func() { defer close(exited); done <- runDaemonWithListen(ctx, "", false, false) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-exited:
				case <-time.After(15 * time.Second):
					t.Error("daemon did not stop")
				}
			})
			var address string
			select {
			case address = <-addresses:
			case <-time.After(10 * time.Second):
				t.Fatal("native ingress was not advertised")
			}
			resp, err := http.Get("http://" + address + "/api/v1/projects")
			require.NoError(t, err)
			_ = resp.Body.Close()
			require.Equal(t, http.StatusNotFound, resp.StatusCode)
			cancel()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(15 * time.Second):
				t.Fatal("daemon did not stop")
			}
			source := federationsigning.Source{KeyID: "key-a", KeyEnv: "TEST_DAEMON_SIGNING_KEY"}
			verifier, err := federationsigning.NewVerifier("https://daemon.example/federation", []federationsigning.Key{{Source: source, EnrollmentID: 1}}, filepath.Join(home, "federation-signing-replay.state"))
			require.NoError(t, err)
			require.NoError(t, verifier.Close())
		})
	}
}
