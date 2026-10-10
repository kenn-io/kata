package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"
)

func normalizeLocalProfileEntry(entry *CatalogDaemonConfig) error {
	entry.Home = strings.TrimSpace(entry.Home)
	entry.InstanceUID = strings.TrimSpace(entry.InstanceUID)
	if entry.Home == "" && entry.InstanceUID == "" {
		return nil
	}
	if !entry.Local {
		return fmt.Errorf("daemon %q: home and instance_uid are only valid for local profiles", entry.Name)
	}
	if entry.Home == "" || entry.InstanceUID == "" {
		return fmt.Errorf("daemon %q: a local profile requires both home and instance_uid", entry.Name)
	}
	if _, err := ulid.ParseStrict(entry.InstanceUID); err != nil {
		return fmt.Errorf("daemon %q: instance_uid must be a valid ULID", entry.Name)
	}
	if strings.HasPrefix(entry.Home, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("daemon %q: resolve user home: %w", entry.Name, err)
		}
		entry.Home = filepath.Join(home, entry.Home[2:])
	}
	if !filepath.IsAbs(entry.Home) {
		return fmt.Errorf("daemon %q: profile home must be an absolute path or start with ~/", entry.Name)
	}
	entry.Home = filepath.Clean(entry.Home)
	return nil
}

// LocalProfileConfig is an immutable selected-home configuration snapshot.
// Resolving it never creates storage or runtime directories.
type LocalProfileConfig struct {
	Name, Home, DSN, StorageID, InstanceUID string
	Config                                  *DaemonConfig
	Catalog                                 CatalogDaemonConfig
}

// ResolveLocalProfile reads only the selected home and derives its storage identity.
func ResolveLocalProfile(entry CatalogDaemonConfig) (LocalProfileConfig, error) {
	if err := normalizeLocalProfileEntry(&entry); err != nil {
		return LocalProfileConfig{}, err
	}
	if !entry.Local || entry.Home == "" {
		return LocalProfileConfig{}, fmt.Errorf("daemon %q is not an explicit local profile", entry.Name)
	}
	cfg, err := ReadDaemonConfigForHome(entry.Home)
	if err != nil {
		return LocalProfileConfig{}, err
	}
	dsn := cfg.Storage.DSN
	if dsn == "" {
		dsn = filepath.Join(entry.Home, "kata.db")
	}
	identity, err := localProfileStorageIdentity(dsn)
	if err != nil {
		return LocalProfileConfig{}, fmt.Errorf("daemon %q: %w", entry.Name, err)
	}
	storageID := DBHash(dsn)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		schema := cfg.Storage.Postgres.Schema
		if schema == "" {
			schema = "kata"
		}
		sum := sha256.Sum256([]byte(identity + "\x00schema=" + schema))
		storageID = hex.EncodeToString(sum[:])[:12]
	}
	return LocalProfileConfig{Name: entry.Name, Home: entry.Home, DSN: dsn, StorageID: storageID, InstanceUID: entry.InstanceUID, Config: cfg, Catalog: entry}, nil
}

// Explicit single-target URLs allow identity inspection and child startup to
// agree without reparsing a credential-free URL through ambient libpq settings.
func localProfileStorageIdentity(dsn string) (string, error) {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		path := strings.TrimPrefix(dsn, "sqlite://")
		if !filepath.IsAbs(path) {
			return "", fmt.Errorf("local profile SQLite storage must be an absolute path")
		}
		return path, nil
	}
	u, err := url.Parse(dsn)
	if err != nil || ambiguousUserinfo(u) {
		return "", fmt.Errorf("local profile PostgreSQL storage must be a valid URL")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 || u.Hostname() == "" || strings.ContainsAny(u.Hostname(), ",/") || u.User == nil || u.User.Username() == "" || strings.TrimPrefix(u.Path, "/") == "" || u.Fragment != "" {
		return "", fmt.Errorf("local profile PostgreSQL storage requires one explicit host, port, database, and user")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid local profile PostgreSQL options")
	}
	for _, key := range []string{"host", "hostaddr", "port", "database", "dbname", "user", "service", "servicefile"} {
		if q.Has(key) {
			return "", fmt.Errorf("local profile PostgreSQL routing option %s is unsupported", key)
		}
	}
	if os.Getenv("PGSERVICE") != "" {
		return "", fmt.Errorf("local profile inspection requires PGSERVICE to be unset")
	}
	ambient := map[string]string{
		"PGPASSWORD": "password", "PGPASSFILE": "passfile", "PGAPPNAME": "application_name", "PGCONNECT_TIMEOUT": "connect_timeout", "PGSSLMODE": "sslmode", "PGSSLKEY": "sslkey", "PGSSLCERT": "sslcert", "PGSSLSNI": "sslsni", "PGSSLROOTCERT": "sslrootcert", "PGSSLPASSWORD": "sslpassword", "PGSSLNEGOTIATION": "sslnegotiation", "PGTARGETSESSIONATTRS": "target_session_attrs", "PGTZ": "timezone", "PGOPTIONS": "options", "PGMINPROTOCOLVERSION": "min_protocol_version", "PGMAXPROTOCOLVERSION": "max_protocol_version",
	}
	for env, key := range ambient {
		explicit := q.Has(key)
		if key == "password" {
			_, explicitPassword := u.User.Password()
			explicit = explicit || explicitPassword
		}
		if os.Getenv(env) != "" && !explicit {
			return "", fmt.Errorf("local profile PostgreSQL option %s must be explicit when %s is set", key, env)
		}
	}
	return CanonicalDSNIdentity(dsn)
}

// localProfileSigningReferences names the federation signing secrets that the
// profile's daemon loads: hub verification keys from its configuration and
// spoke signing sources saved in its home's credentials file.
func localProfileSigningReferences(profile LocalProfileConfig) ([]string, error) {
	var references []string
	if profile.Config != nil {
		for _, key := range profile.Config.Federation.Signing.Keys {
			references = append(references, key.KeyEnv)
		}
	}
	credentials, err := readFederationCredentialsFile(filepath.Join(profile.Home, "credentials.toml"))
	if err != nil {
		return nil, fmt.Errorf("read local profile federation credentials: %w", err)
	}
	for _, credential := range credentials.Projects {
		if credential.Signing != nil {
			references = append(references, credential.Signing.KeyEnv)
		}
	}
	return references, nil
}

// LocalProfileEnvironment constructs child state from an OS/toolchain allowlist
// and credential references in the selected configuration, never parent daemon
// overrides. The returned slice does not mutate the process environment.
func LocalProfileEnvironment(profile LocalProfileConfig, autostart bool) ([]string, error) {
	values := map[string]string{}
	for _, key := range []string{"PATH", "HOME", "USER", "LOGNAME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "SystemRoot", "SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT", "TMP", "TEMP", "TMPDIR", "XDG_RUNTIME_DIR", "LANG", "LC_ALL", "TZ", "GOCACHE", "GOMODCACHE", "GOPATH", "KATA_TELEMETRY_ENABLED", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	references := []string{profile.Catalog.TokenEnv}
	if profile.Config != nil {
		references = append(references,
			profile.Config.Auth.TokenEnv,
			profile.Config.Search.Embeddings.APIKeyEnv,
			profile.Config.GitHubSync.TokenEnvName(),
			profile.Config.NotionSync.TokenEnv,
			profile.Config.PlaneSync.TokenEnv,
			profile.Config.LinearSync.TokenEnv,
			profile.Config.TwentySync.TokenEnv,
			profile.Config.TodoistSync.TokenEnv,
			profile.Config.TickTickSync.TokenEnv,
		)
		for _, entry := range profile.Config.Daemons {
			references = append(references, entry.TokenEnv)
		}
		for _, connector := range profile.Config.Connectors {
			for _, source := range connector.Env {
				references = append(references, source)
			}
		}
	}
	signingReferences, err := localProfileSigningReferences(profile)
	if err != nil {
		return nil, err
	}
	references = append(references, signingReferences...)
	reserved := []string{
		"KATA_LISTEN", "KATA_WEB_LISTEN", "KATA_WEB_PUBLIC_ORIGIN", "KATA_AUTH_TOKEN_FILE",
		"KATA_SEARCH_EMBEDDINGS_BASE_URL", "KATA_SEARCH_EMBEDDINGS_MODEL", "KATA_SEARCH_EMBEDDINGS_DIMS", "KATA_SEARCH_EMBEDDINGS_API_KEY_FILE",
		"KATA_SEARCH_EMBEDDINGS_DOCUMENT_PREFIX", "KATA_SEARCH_EMBEDDINGS_DOCUMENT_SUFFIX",
		"KATA_SEARCH_EMBEDDINGS_QUERY_PREFIX", "KATA_SEARCH_EMBEDDINGS_QUERY_SUFFIX",
		"KATA_SEARCH_EMBEDDINGS_REQUEST_DIMENSIONS",
		"KATA_BACKUP_DIR", "KATA_BACKUP_INTERVAL", "KATA_BACKUP_RETAIN",
		"KATA_HOME", "KATA_DB", "KATA_DSN", "KATA_SERVER", "KATA_AUTH_TOKEN", "KATA_ALLOW_INSECURE",
		"KATA_AUTOSTART", "KATA_AUTOSTART_IDLE_TIMEOUT", "KATA_SKIP_DAEMON_VERSION_CHECK", "KATA_HTTP_TIMEOUT",
		"KATA_POSTGRES_SCHEMA", "KATA_POSTGRES_SCHEMA_MODE", "KATA_POSTGRES_SCHEMA_OWNER", "KATA_POSTGRES_ALLOW_INSECURE",
		"KATA_TRUST_PRIVATE_NETWORK", "KATA_ALLOW_UNAUTHENTICATED_PRIVATE_NETWORK_WRITES",
		"KATA_ALLOW_IDENTITY_CONNECTOR_ADMINISTRATION", "KATA_TRUSTED_ACTOR_HEADER", "KATA_TRUSTED_PROXY_LISTENERS",
		"KATA_WEB_ALLOWED_HOSTS", "KATA_FEDERATION_PULL_INTERVAL_MS", "KATA_GITHUB_SYNC_INTERVAL_MS",
		"KATA_AUTHOR", "KATA_TEAMMATE", "KATA_INBOX_USER", "KATA_REF", "KATA_COLOR_MODE", "KATA_TELEMETRY_ENABLED",
		"KATA_TEST_FEDERATION_FAILPOINTS", "KATA_GITHUB_SYNC_ALLOWED_HOSTS",
	}
	copied := map[string]bool{}
	for _, key := range references {
		if key == "" || copied[key] {
			continue
		}
		copied[key] = true
		upper := strings.ToUpper(key)
		_, osKey := values[key]
		if !environmentNamePattern.MatchString(key) || osKey || strings.HasPrefix(upper, "PG") || slices.Contains(reserved, upper) || upper == "PORT" || strings.HasSuffix(upper, "_PROXY") || upper == "ALL_PROXY" || upper == "NO_PROXY" {
			return nil, fmt.Errorf("local profile credential reference %q names a reserved environment key", key)
		}
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	values["KATA_HOME"] = profile.Home
	if profile.DSN != "" {
		values["KATA_DSN"] = profile.DSN
		if strings.HasPrefix(profile.DSN, "postgres://") || strings.HasPrefix(profile.DSN, "postgresql://") {
			schema := profile.Config.Storage.Postgres.Schema
			if schema == "" {
				schema = "kata"
			}
			values["KATA_POSTGRES_SCHEMA"] = schema
		}
	}
	if autostart {
		values["KATA_AUTOSTART"] = "1"
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result, nil
}
