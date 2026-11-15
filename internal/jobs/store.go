package jobs

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // SQLite のドライバー。CGO を使わない。
)

// Store はジョブの履歴を SQLite に置く。消えても、コピーや削除の動作には影響しない。
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS jobs (
	id            TEXT PRIMARY KEY,
	source        TEXT NOT NULL,
	path          BLOB NOT NULL,
	library       TEXT NOT NULL,
	status        TEXT NOT NULL,
	error_code    TEXT NOT NULL DEFAULT '',
	error_message TEXT NOT NULL DEFAULT '',
	bytes_total   INTEGER NOT NULL DEFAULT 0,
	bytes_done    INTEGER NOT NULL DEFAULT 0,
	files_total   INTEGER NOT NULL DEFAULT 0,
	files_done    INTEGER NOT NULL DEFAULT 0,
	resync        INTEGER NOT NULL DEFAULT 0,
	retry_of      TEXT NOT NULL DEFAULT '',
	created_at    INTEGER NOT NULL,
	started_at    INTEGER NOT NULL DEFAULT 0,
	finished_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS jobs_created_at ON jobs (created_at);
`

// OpenStore は path の SQLite を開く。path が ":memory:" ならメモリに置く。
func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file::memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init state db: %w", err)
	}
	return &Store{db: db}, nil
}

// Close は DB を閉じる。
func (s *Store) Close() error { return s.db.Close() }

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// Save はジョブを書き込む。
func (s *Store) Save(j *Job) error {
	_, err := s.db.Exec(`INSERT INTO jobs (id, source, path, library, status, error_code, error_message,
		bytes_total, bytes_done, files_total, files_done, resync, retry_of, created_at, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET status = excluded.status, error_code = excluded.error_code,
		error_message = excluded.error_message, bytes_total = excluded.bytes_total, bytes_done = excluded.bytes_done,
		files_total = excluded.files_total, files_done = excluded.files_done, resync = excluded.resync,
		started_at = excluded.started_at, finished_at = excluded.finished_at`,
		j.ID, j.Source, []byte(j.Rel), j.Library, j.Status, j.ErrorCode, j.ErrorMessage,
		j.BytesTotal, j.BytesDone, j.FilesTotal, j.FilesDone, j.Resync, j.RetryOf,
		ms(j.CreatedAt), ms(j.StartedAt), ms(j.FinishedAt))
	return err
}

// Delete はジョブを消す。
func (s *Store) Delete(ids []string) error {
	var errs []error
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// LoadAll はすべてのジョブを古い順に返す。
func (s *Store) LoadAll() ([]*Job, error) {
	rows, err := s.db.Query(`SELECT id, source, path, library, status, error_code, error_message,
		bytes_total, bytes_done, files_total, files_done, resync, retry_of, created_at, started_at, finished_at
		FROM jobs ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	for rows.Next() {
		var (
			j                Job
			path             []byte
			created, started int64
			finished         int64
		)
		if err := rows.Scan(&j.ID, &j.Source, &path, &j.Library, &j.Status, &j.ErrorCode, &j.ErrorMessage,
			&j.BytesTotal, &j.BytesDone, &j.FilesTotal, &j.FilesDone, &j.Resync, &j.RetryOf,
			&created, &started, &finished); err != nil {
			return nil, err
		}
		j.Rel = string(path)
		j.CreatedAt, j.StartedAt, j.FinishedAt = fromMS(created), fromMS(started), fromMS(finished)
		out = append(out, &j)
	}
	return out, rows.Err()
}
