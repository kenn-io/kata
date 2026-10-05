package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"go.kenn.io/kata/internal/federationsigning"
)

// FederationSigningConfig applies hub-controlled policy on every listener.
type FederationSigningConfig struct {
	Required        bool                    `toml:"required"`
	ExternalURL     string                  `toml:"external_url"`
	ReplayStateFile string                  `toml:"replay_state_file"`
	Keys            []federationsigning.Key `toml:"key"`
}

// FederationIngressConfig enables the separate private federation listener.
type FederationIngressConfig struct {
	Enabled bool   `toml:"enabled"`
	Listen  string `toml:"listen"`
}

// normalizeFederationSigning validates signing policy. home is the directory
// of the configuration file being read and owns the default replay state.
func normalizeFederationSigning(cfg *DaemonConfig, home string) error {
	s := &cfg.Federation.Signing
	if s.Required || s.ExternalURL != "" || len(s.Keys) > 0 || s.ReplayStateFile != "" {
		if s.ExternalURL == "" {
			return errors.New("federation.signing.external_url is required when federation signing is configured")
		}
		if len(s.Keys) == 0 {
			return errors.New("federation.signing.key entries are required when federation signing is configured")
		}
		if err := federationsigning.ValidatePolicyShape(s.ExternalURL, s.Keys); err != nil {
			return err
		}
		if s.ReplayStateFile == "" {
			absoluteHome, err := filepath.Abs(home)
			if err != nil {
				return fmt.Errorf("resolve federation signing replay state home: %w", err)
			}
			s.ReplayStateFile = filepath.Join(absoluteHome, "federation-signing-replay.state")
		}
		if !filepath.IsAbs(s.ReplayStateFile) {
			return errors.New("federation.signing.replay_state_file must be absolute")
		}
		active := map[int64]int{}
		total := map[int64]int{}
		for _, k := range s.Keys {
			total[k.EnrollmentID]++
			if k.NotAfter == 0 {
				active[k.EnrollmentID]++
			} else if k.NotAfter > time.Now().Add(24*time.Hour).Unix() {
				return errors.New("federation signing rotation overlap must end within 24 hours")
			}
		}
		for id, count := range total {
			if count > 2 || active[id] != 1 {
				return errors.New("federation signing requires one active key and at most one retiring key per enrollment")
			}
		}
	}
	i := &cfg.Federation.Ingress
	if !i.Enabled {
		if i.Listen != "" {
			return errors.New("federation.ingress.listen requires enabled=true")
		}
		return nil
	}
	if s.ExternalURL == "" {
		return errors.New("federation.ingress requires federation signing configuration")
	}
	if i.Listen == "" {
		i.Listen = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(i.Listen)
	if err != nil {
		return errors.New("federation.ingress.listen must be a literal private IP and port")
	}
	ip := net.ParseIP(host)
	p, err := strconv.Atoi(port)
	if ip == nil || ip.IsUnspecified() || err != nil || p < 0 || p > 65535 {
		return errors.New("federation.ingress.listen must be a literal private IP and port")
	}
	if err := (BearerPolicy{TrustPrivateNetwork: cfg.Auth.TrustPrivateNetwork}).CheckTargetURL(&url.URL{Scheme: "http", Host: i.Listen}); err != nil {
		return err
	}
	return nil
}
