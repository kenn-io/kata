package main

import (
	"context"

	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/daemonruntime"
	kitdaemon "go.kenn.io/kit/daemon"
)

func requireUnmanagedCurrentDaemon(ctx context.Context) error {
	home, err := config.KataHome()
	if err != nil {
		return err
	}
	return config.RequireUnmanagedDaemonHome(ctx, home)
}

func requireUnmanagedRecords(records []kitdaemon.RuntimeRecord) error {
	for _, record := range records {
		if mode := record.Metadata[daemonruntime.RuntimeSocketActivationKey]; mode != "" {
			return config.SocketActivationControlError(mode)
		}
	}
	return nil
}
