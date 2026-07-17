package kata_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExternalModuleCanServeAndStop(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repositoryRoot := filepath.Dir(currentFile)
	consumerDir := t.TempDir()

	goMod := fmt.Sprintf(`module example.com/kata-service-consumer

go 1.26.3

require go.kenn.io/kata v0.0.0

replace go.kenn.io/kata => %s
`, filepath.ToSlash(repositoryRoot))
	require.NoError(t, os.WriteFile(filepath.Join(consumerDir, "go.mod"), []byte(goMod), 0o600))
	require.NoError(t, os.WriteFile(
		filepath.Join(consumerDir, "service_test.go"),
		[]byte(externalConsumerTest),
		0o600,
	))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=mod", "./...")
	cmd.Dir = consumerDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	assert.NoError(t, err, string(output))
}

const externalConsumerTest = `package consumer_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"go.kenn.io/kata"
)

func TestMountedServiceLifecycle(t *testing.T) {
	service, err := kata.New(context.Background(), kata.Config{
		DSN:  filepath.Join(t.TempDir(), "kata.db"),
		Auth: kata.AuthConfig{TrustCallerAuthentication: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("health status = %d, body = %s", response.Code, response.Body.String())
	}

	runContext, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- service.Run(runContext) }()
	cancelRun()
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}
`
