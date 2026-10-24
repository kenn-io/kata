package client

import "go.kenn.io/kata/internal/config"

// CatalogDaemon is one [[daemon]] entry in <KATA_HOME>/config.toml. It carries
// no credentials.
type CatalogDaemon struct {
	Name string
	// Local is true for a local entry, including a pinned local profile.
	Local bool
	// Endpoint is "local" for a local entry and the URL's host[:port] for a
	// remote one. It is for display; DiscoverOptions.Daemon connects by Name.
	Endpoint string
	// HasToken reports that the entry sets token or token_env. The variable
	// token_env names is not read, so it can still be unset.
	HasToken bool
	// Active reports that active_daemon names this entry.
	Active bool
}

// DaemonCatalog returns the daemon catalog in file order, as kata tui's daemon
// switcher lists it. A missing config file yields no entries; an invalid
// catalog returns kata's configuration error.
func DaemonCatalog() ([]CatalogDaemon, error) {
	cfg, err := config.ReadDaemonCatalogAndAuthPolicy()
	if err != nil {
		return nil, err
	}
	daemons := make([]CatalogDaemon, 0, len(cfg.Daemons))
	for _, d := range cfg.Daemons {
		daemons = append(daemons, CatalogDaemon{
			Name:     d.Name,
			Local:    d.Local,
			Endpoint: d.DisplayEndpoint(),
			HasToken: d.HasClientToken(),
			Active:   d.Name == cfg.ActiveDaemon,
		})
	}
	return daemons, nil
}
