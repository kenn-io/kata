package pgstore

import (
	"context"
	"database/sql/driver"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.kenn.io/kata/internal/db"
)

// federationPollWakeChannel is shared by every schema in a database; each
// notification names its schema so other installations ignore it.
const federationPollWakeChannel = "kata_federation_poll"

var _ db.FederationPollWakeBus = (*Store)(nil)

// pollWakeClosed reports whether Close has run.
func (s *Store) pollWakeClosed() bool {
	return s.pollWakeCtx.Err() != nil
}

type federationPollWakeNotification struct {
	Schema  string `json:"schema"`
	Payload string `json:"payload"`
}

// PublishFederationPollWake notifies every hub process listening on this
// database and schema.
func (s *Store) PublishFederationPollWake(ctx context.Context, payload string) error {
	if s.pollWakeDB == nil || s.pollWakeClosed() {
		return db.ErrFederationPollWakeClosed
	}
	message, err := json.Marshal(federationPollWakeNotification{Schema: s.schema, Payload: payload})
	if err != nil {
		return err
	}
	if _, err := s.pollWakeDB.ExecContext(ctx, `SELECT pg_notify($1,$2)`, federationPollWakeChannel, string(message)); err != nil {
		return s.pollWakeError("publish federation poll wake", err)
	}
	return nil
}

// ListenFederationPollWake holds one dedicated connection in LISTEN until ctx
// ends or the connection fails.
func (s *Store) ListenFederationPollWake(ctx context.Context, listening func(), deliver func(payload string)) error {
	if s.pollWakeDB == nil {
		return db.ErrFederationPollWakeClosed
	}
	if s.pollWakeClosed() {
		return db.ErrFederationPollWakeClosed
	}
	// Closing the pool does not interrupt a connection in use, so Close ends
	// the listener through its context.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.pollWakeCtx, cancel)()
	conn, err := s.pollWakeDB.Conn(ctx)
	if err != nil {
		return s.pollWakeError("open federation poll wake listener", err)
	}
	defer func() { _ = conn.Close() }()
	err = conn.Raw(func(driverConn any) error {
		// The connection holds LISTEN, so it must never return to the pool.
		return errors.Join(listenFederationPollWake(ctx, s.schema, driverConn, listening, deliver), driver.ErrBadConn)
	})
	return s.pollWakeError("listen for federation poll wakes", err)
}

// pollWakeError reports a failure after Close as the bus being closed, so the
// bridge stops instead of retrying.
func (s *Store) pollWakeError(action string, err error) error {
	if err == nil {
		return nil
	}
	if s.pollWakeClosed() {
		return errors.Join(db.ErrFederationPollWakeClosed, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

func listenFederationPollWake(ctx context.Context, schema string, driverConn any, listening func(), deliver func(payload string)) error {
	raw, ok := driverConn.(*stdlib.Conn)
	if !ok {
		return fmt.Errorf("unexpected postgres driver connection %T", driverConn)
	}
	listener := raw.Conn()
	if _, err := listener.Exec(ctx, "LISTEN "+pgx.Identifier{federationPollWakeChannel}.Sanitize()); err != nil {
		return fmt.Errorf("listen for federation poll wakes: %w", err)
	}
	listening()
	for {
		notification, err := listener.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for federation poll wake: %w", err)
		}
		var message federationPollWakeNotification
		if json.Unmarshal([]byte(notification.Payload), &message) != nil || message.Schema != schema {
			continue
		}
		deliver(message.Payload)
	}
}
