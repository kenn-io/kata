package client_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pkg/client is linked into client-only programs. It must not pull in the
// daemon, storage engines, telemetry, connectors, or federation signing.
// Extending either list grows every client binary; prefer moving the needed
// code into a leaf package instead.
var (
	allowedKataPackages = []string{
		"go.kenn.io/kata/internal/client",
		"go.kenn.io/kata/internal/config",
		"go.kenn.io/kata/internal/daemonruntime",
		"go.kenn.io/kata/internal/federationsigning/signingpolicy",
		"go.kenn.io/kata/internal/httpurl",
		"go.kenn.io/kata/internal/linkpath",
		"go.kenn.io/kata/internal/processtree",
		"go.kenn.io/kata/internal/projectscope",
		"go.kenn.io/kata/internal/tokenactor",
		"go.kenn.io/kata/internal/transcript",
		"go.kenn.io/kata/internal/uid",
		"go.kenn.io/kata/internal/version",
		"go.kenn.io/kata/pkg/client",
		"go.kenn.io/kata/pkg/client/generated",
		"go.kenn.io/kata/pkg/federationprovider",
	}
	allowedModules = []string{
		"github.com/BurntSushi/toml",
		"github.com/doordash-oss/oapi-codegen-dd/v3",
		"github.com/ebitengine/purego",
		"github.com/gabriel-vasile/mimetype",
		"github.com/go-ole/go-ole",
		"github.com/go-playground/locales",
		"github.com/go-playground/universal-translator",
		"github.com/go-playground/validator/v10",
		"github.com/gofrs/flock",
		"github.com/google/uuid",
		"github.com/leodido/go-urn",
		"github.com/oklog/ulid/v2",
		"github.com/shirou/gopsutil/v4",
		"github.com/spf13/pathologize",
		"github.com/tklauser/go-sysconf",
		"github.com/tklauser/numcpus",
		"github.com/yusufpapurcu/wmi",
		"go.kenn.io/kata",
		"go.kenn.io/kit",
		"go.yaml.in/yaml/v4",
		"golang.org/x/crypto",
		"golang.org/x/mod",
		"golang.org/x/sync",
		"golang.org/x/sys",
		"golang.org/x/text",
	}
)

func TestClientPackageLinksOnlyClientDependencies(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go list for each target OS")
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "go", "list", "-deps",
				"-f", "{{if not .Standard}}{{.ImportPath}} {{with .Module}}{{.Path}}{{end}}{{end}}",
				"go.kenn.io/kata/pkg/client")
			cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH=amd64", "CGO_ENABLED=0")
			output, err := cmd.Output()
			require.NoError(t, err)

			var unexpected []string
			for line := range strings.Lines(string(output)) {
				pkg, module, _ := strings.Cut(strings.TrimSpace(line), " ")
				if pkg == "" {
					continue
				}
				if module == "go.kenn.io/kata" && !slices.Contains(allowedKataPackages, pkg) ||
					!slices.Contains(allowedModules, module) {
					unexpected = append(unexpected, pkg)
				}
			}
			assert.Empty(t, unexpected, "pkg/client links packages outside its client-side allowlist")
		})
	}
}
