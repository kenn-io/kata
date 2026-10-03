package main

import (
	"context"
	"fmt"
	"log"

	"go.kenn.io/kata/internal/backup"
	"go.kenn.io/kata/internal/config"
	"go.kenn.io/kata/internal/db"
	"go.kenn.io/kata/internal/jsonl"
)

func startDaemonBackups(
	ctx context.Context, workers *daemonWorkerGroup, store db.Storage,
	cfg config.BackupConfig, storageID string, logger *log.Logger,
) error {
	interval, retain, err := cfg.Durations()
	if err != nil {
		return err
	}
	if cfg.Dir == "" {
		return nil
	}
	schedule, err := backup.New(backup.Settings{
		Dir: cfg.Dir, StorageID: storageID, Interval: interval, Retain: retain,
		Export: func(ctx context.Context, path string) error {
			return writeExportOutput(ctx, store, path, jsonl.ExportOptions{IncludeDeleted: true})
		},
	})
	if err != nil {
		return err
	}
	if !workers.Go(func() {
		schedule.Run(ctx, backup.SystemClock{}, func(err error) { logger.Printf("scheduled backup: %v", err) })
	}) {
		return fmt.Errorf("daemon is shutting down before backup scheduling")
	}
	return nil
}
