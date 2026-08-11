package mqtt

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"fmt"
	"time"

	"github.com/lsongdev/mqtt-go/proto"
	_ "modernc.org/sqlite"
)

// SQLiteSessionStore persists MQTT sessions in one SQLite database. The
// schema is intentionally private and compact; callers interact through the
// SessionStore interface.
type SQLiteSessionStore struct{ db *sql.DB }

// OpenSQLiteSessionStore opens or creates a SQLite-backed session store.
func OpenSQLiteSessionStore(path string) (*SQLiteSessionStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS mqtt_sessions (
		client_id TEXT PRIMARY KEY,
		expires_at INTEGER NOT NULL,
		data BLOB NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("mqtt: initialize sqlite session store: %w", err)
	}
	return &SQLiteSessionStore{db: db}, nil
}

func init() {
	gob.Register(proto.VarInt(0))
	gob.Register(proto.StringPair{})
	gob.Register([]byte{})
}

func encodeSession(s StoredSession) ([]byte, error) {
	var b bytes.Buffer
	err := gob.NewEncoder(&b).Encode(s)
	return b.Bytes(), err
}
func decodeSession(data []byte) (StoredSession, error) {
	var s StoredSession
	err := gob.NewDecoder(bytes.NewReader(data)).Decode(&s)
	return s, err
}

func (s *SQLiteSessionStore) List(ctx context.Context) ([]StoredSession, error) {
	now := time.Now().UnixNano()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM mqtt_sessions WHERE expires_at != 0 AND expires_at <= ?`, now); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT data FROM mqtt_sessions ORDER BY client_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []StoredSession
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		session, err := decodeSession(data)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *SQLiteSessionStore) Save(ctx context.Context, session StoredSession) error {
	data, err := encodeSession(session)
	if err != nil {
		return err
	}
	expires := int64(0)
	if !session.ExpiresAt.IsZero() {
		expires = session.ExpiresAt.UnixNano()
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO mqtt_sessions(client_id,expires_at,data) VALUES(?,?,?)
		ON CONFLICT(client_id) DO UPDATE SET expires_at=excluded.expires_at,data=excluded.data`, session.ClientID, expires, data)
	return err
}

func (s *SQLiteSessionStore) Delete(ctx context.Context, clientID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mqtt_sessions WHERE client_id=?`, clientID)
	return err
}
func (s *SQLiteSessionStore) Close() error { return s.db.Close() }
