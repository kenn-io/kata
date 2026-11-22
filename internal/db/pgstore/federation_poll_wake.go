package pgstore

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"database/sql/driver"

	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.kenn.io/kata/internal/db"
)

// PostgreSQL permits every connected role to listen and notify on any channel.
// The instance UID and schema distinguish installations; they are not secrets.
// The federation bridge sends only the publishing process's instance tag.
// Connected roles can observe wake activity. Each receiving poll rechecks
// authorization before reading its project.
const (
	pollWakeChannelInfo = "kata federation poll wake channel v1\x00"
	// pollWakeChannelBytes keeps the hex channel name within PostgreSQL's
	// 63-byte identifier limit.
	pollWakeChannelBytes = 24
	// pollWakeMaxNotifyBytes is PostgreSQL's limit on a NOTIFY payload.
	pollWakeMaxNotifyBytes = 7999
)

var _ db.FederationPollWakeBus = (*Store)(nil)

// pollWakeClosed reports whether Close has run.
func (s *Store) pollWakeClosed() bool {
	return s.pollWakeCtx.Err() != nil
}

// pollWakeChannel derives the channel name for this schema.
func (s *Store) pollWakeChannel() (string, error) {
	if s.instanceUID == "" {
		return "", errors.New("federation poll wake needs the instance UID")
	}
	channel, err := hkdf.Key(sha256.New, []byte(s.instanceUID), nil, pollWakeChannelInfo+s.schema, pollWakeChannelBytes)
	if err != nil {
		return "", fmt.Errorf("derive federation poll wake channel: %w", err)
	}
	return hex.EncodeToString(channel), nil
}

// PublishFederationPollWake notifies every hub process listening on this
// database and schema.
func (s *Store) PublishFederationPollWake(ctx context.Context, payload string) error {
	if s.pollWakeDB == nil || s.pollWakeClosed() {
		return db.ErrFederationPollWakeClosed
	}
	channel, err := s.pollWakeChannel()
	if err != nil {
		return err
	}
	if len(payload) > pollWakeMaxNotifyBytes {
		return fmt.Errorf("federation poll wake payload of %d bytes exceeds the NOTIFY limit", len(payload))
	}
	if _, err := s.pollWakeDB.ExecContext(ctx, `SELECT pg_notify($1,$2)`, channel, payload); err != nil {
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
	channel, err := s.pollWakeChannel()
	if err != nil {
		return err
	}
	conn, err := s.pollWakeDB.Conn(ctx)
	if err != nil {
		return s.pollWakeError("open federation poll wake listener", err)
	}
	defer func() { _ = conn.Close() }()
	err = conn.Raw(func(driverConn any) error {
		// The connection holds LISTEN, so it must never return to the pool.
		return errors.Join(listenFederationPollWake(ctx, channel, driverConn, listening, deliver), driver.ErrBadConn)
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

func listenFederationPollWake(ctx context.Context, channel string, driverConn any, listening func(), deliver func(payload string)) error {
	raw, ok := driverConn.(*stdlib.Conn)
	if !ok {
		return fmt.Errorf("unexpected postgres driver connection %T", driverConn)
	}
	listener := raw.Conn()
	if _, err := listener.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		return fmt.Errorf("listen for federation poll wakes: %w", err)
	}
	listening()
	for {
		notification, err := listener.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("wait for federation poll wake: %w", err)
		}
		deliver(notification.Payload)
	}
}
