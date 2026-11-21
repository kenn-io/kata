package pgstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"go.kenn.io/kata/internal/db"
)

// LISTEN and NOTIFY carry no privileges: any role that can connect to the
// database can listen on a channel or notify it. The channel name and an AES-GCM
// key are therefore derived from meta.instance_uid, which a role without access
// to this schema cannot read, so such a role can neither find the channel, read
// which project committed, nor wake held polls. The schema name is part of the
// derivation so schemas that share an instance UID, as an import can leave
// them, stay apart.
const (
	pollWakeChannelInfo = "kata federation poll wake channel v1\x00"
	pollWakeKeyInfo     = "kata federation poll wake key v1\x00"
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

// pollWakeKeys derives the channel name and payload cipher for this schema.
func (s *Store) pollWakeKeys() (string, cipher.AEAD, error) {
	if s.instanceUID == "" {
		return "", nil, errors.New("federation poll wake needs the instance UID")
	}
	channel, err := hkdf.Key(sha256.New, []byte(s.instanceUID), nil, pollWakeChannelInfo+s.schema, pollWakeChannelBytes)
	if err != nil {
		return "", nil, fmt.Errorf("derive federation poll wake channel: %w", err)
	}
	key, err := hkdf.Key(sha256.New, []byte(s.instanceUID), nil, pollWakeKeyInfo+s.schema, 32)
	if err != nil {
		return "", nil, fmt.Errorf("derive federation poll wake key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", nil, fmt.Errorf("federation poll wake cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", nil, fmt.Errorf("federation poll wake cipher: %w", err)
	}
	return hex.EncodeToString(channel), aead, nil
}

// PublishFederationPollWake notifies every hub process listening on this
// database and schema.
func (s *Store) PublishFederationPollWake(ctx context.Context, payload string) error {
	if s.pollWakeDB == nil || s.pollWakeClosed() {
		return db.ErrFederationPollWakeClosed
	}
	channel, aead, err := s.pollWakeKeys()
	if err != nil {
		return err
	}
	plaintext := []byte(payload)
	nonce := make([]byte, aead.NonceSize(), aead.NonceSize()+len(plaintext)+aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate federation poll wake nonce: %w", err)
	}
	message := base64.RawStdEncoding.EncodeToString(aead.Seal(nonce, nonce, plaintext, nil))
	if len(message) > pollWakeMaxNotifyBytes {
		return fmt.Errorf("federation poll wake payload of %d bytes exceeds the NOTIFY limit", len(payload))
	}
	if _, err := s.pollWakeDB.ExecContext(ctx, `SELECT pg_notify($1,$2)`, channel, message); err != nil {
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
	channel, aead, err := s.pollWakeKeys()
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
		return errors.Join(listenFederationPollWake(ctx, channel, aead, driverConn, listening, deliver), driver.ErrBadConn)
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

func listenFederationPollWake(ctx context.Context, channel string, aead cipher.AEAD, driverConn any, listening func(), deliver func(payload string)) error {
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
		sealed, err := base64.RawStdEncoding.DecodeString(notification.Payload)
		if err != nil {
			continue
		}
		if len(sealed) < aead.NonceSize() {
			continue
		}
		plaintext, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
		if err != nil {
			continue
		}
		deliver(string(plaintext))
	}
}
