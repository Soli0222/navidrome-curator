// Package jobs は、アルバムを library にコピーするジョブの待ち行列と実行をまとめる。
//
// ジョブの状態はプロセスの中に持ち、履歴を SQLite に書く。
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Soli0222/navidrome-curator/internal/library"
	"github.com/Soli0222/navidrome-curator/internal/scan"
)

// ジョブの状態。
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// 失敗の理由。画面はこのコードで文言を選ぶ。
const (
	CodeInsufficientSpace = "insufficient_space"
	CodeSourceRead        = "source_read"
	CodeSizeMismatch      = "size_mismatch"
	CodeNameInvalid       = "name_invalid"
	CodeDestinationWrite  = "destination_write"
	CodeNotFound          = "not_found"
	CodeInvalidPath       = "invalid_path"
	CodeInterrupted       = "interrupted"
	CodeCanceled          = "canceled"
	CodeInternal          = "internal"
)

var (
	// ErrDuplicate は、同じ library と相対パスのジョブがすでに待ちか実行中であることを表す。
	ErrDuplicate = errors.New("a job for the same album and library is already queued or running")
	// ErrNotFound はジョブがないことを表す。
	ErrNotFound = errors.New("job not found")
	// ErrState は、今の状態ではできない操作であることを表す。
	ErrState = errors.New("operation not allowed in the current state")
)

// Job は1つのアルバムを1つの library にコピーするジョブ。
type Job struct {
	ID           string    `json:"id"`
	Source       string    `json:"source"`
	Rel          string    `json:"path"`
	Library      string    `json:"library"`
	Status       string    `json:"status"`
	ErrorCode    string    `json:"errorCode,omitempty"`
	ErrorMessage string    `json:"errorMessage,omitempty"`
	BytesTotal   int64     `json:"bytesTotal"`
	BytesDone    int64     `json:"bytesDone"`
	FilesTotal   int       `json:"filesTotal"`
	FilesDone    int       `json:"filesDone"`
	Resync       bool      `json:"resync"`
	RetryOf      string    `json:"retryOf,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	StartedAt    time.Time `json:"startedAt,omitzero"`
	FinishedAt   time.Time `json:"finishedAt,omitzero"`
}

// Finished は、ジョブが終わっているかを返す。
func (j *Job) Finished() bool {
	return j.Status == StatusCompleted || j.Status == StatusFailed || j.Status == StatusCanceled
}

// Hooks はジョブの状態が変わったときに呼ぶ関数。メトリクスと通知に使う。
type Hooks struct {
	OnFinished    func(j Job, duration time.Duration)
	OnCopiedBytes func(n int64)
	OnQueueChange func(queued int)
}

// Options は Manager の設定。
type Options struct {
	Sources         map[string]string // source の名前 -> パス
	LibraryRoot     string
	StagingDir      string
	FreeSpaceMargin int64
	FileMode        fs.FileMode
	DirMode         fs.FileMode
	Concurrency     int
	HistoryLimit    int
	TrashBytes      func() int64
}

type entry struct {
	job    Job
	cancel context.CancelFunc
	// 実行中の進み具合。job の値とは別に、ロックなしで更新する。
	bytesDone atomic.Int64
	filesDone atomic.Int64
}

// Manager はジョブを持ち、順に実行する。
type Manager struct {
	opt   Options
	rules *scan.Rules
	libs  *library.Manager
	store *Store
	log   *slog.Logger
	hooks Hooks
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
	order   []string // 作った順
	wake    chan struct{}
	wg      sync.WaitGroup

	// テスト用。ファイルを1つコピーするたびに呼ぶ。
	afterFile func(j *Job, rel string) error
}

// New は Manager を作る。Start を呼ぶまでジョブは動かない。
func New(opt Options, rules *scan.Rules, libs *library.Manager, store *Store, log *slog.Logger, hooks Hooks) *Manager {
	if opt.Concurrency < 1 {
		opt.Concurrency = 1
	}
	if opt.HistoryLimit < 1 {
		opt.HistoryLimit = 500
	}
	return &Manager{
		opt: opt, rules: rules, libs: libs, store: store, log: log, hooks: hooks, now: time.Now,
		entries: map[string]*entry{}, wake: make(chan struct{}, 1),
	}
}

// CleanStaging は staging に残っているものをすべて消す。
func CleanStaging(dir string, mode fs.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, de := range des {
		if err := os.RemoveAll(filepath.Join(dir, de.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Start は staging を片付け、履歴を読み、ワーカーを動かす。
func (m *Manager) Start(ctx context.Context) error {
	if err := CleanStaging(m.opt.StagingDir, m.opt.DirMode); err != nil {
		return err
	}
	if m.store != nil {
		saved, err := m.store.LoadAll()
		if err != nil {
			m.log.Error("failed to load job history; starting with an empty history", "error", err)
		}
		m.mu.Lock()
		for _, j := range saved {
			if j.Status == StatusRunning {
				// 前のプロセスが止まったときに実行中だったもの。
				j.Status = StatusFailed
				j.ErrorCode = CodeInterrupted
				j.ErrorMessage = "the process stopped while this job was running"
				j.FinishedAt = m.now()
				m.persist(j)
			}
			m.entries[j.ID] = &entry{job: *j}
			m.order = append(m.order, j.ID)
		}
		m.pruneLocked()
		m.mu.Unlock()
	}
	m.notifyQueue()
	for i := 0; i < m.opt.Concurrency; i++ {
		m.wg.Add(1)
		go m.worker(ctx)
	}
	m.signal()
	return nil
}

// Wait はワーカーが止まるのを待つ。
func (m *Manager) Wait() { m.wg.Wait() }

func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func newID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// Enqueue はジョブを作り、待ち行列に入れる。同じ library と相対パスのジョブが待ちか実行中なら、それを返して ErrDuplicate にする。
func (m *Manager) Enqueue(source, rel, lib string) (Job, error) {
	return m.enqueue(source, rel, lib, "")
}

func (m *Manager) enqueue(source, rel, lib, retryOf string) (Job, error) {
	m.mu.Lock()
	for _, id := range m.order {
		e := m.entries[id]
		if e.job.Library == lib && e.job.Rel == rel && !e.job.Finished() {
			j := m.snapshotLocked(e)
			m.mu.Unlock()
			return j, ErrDuplicate
		}
	}
	now := m.now()
	j := Job{ID: newID(now), Source: source, Rel: rel, Library: lib, Status: StatusQueued, CreatedAt: now, RetryOf: retryOf}
	e := &entry{job: j}
	m.entries[j.ID] = e
	m.order = append(m.order, j.ID)
	m.persist(&e.job)
	m.mu.Unlock()
	m.notifyQueue()
	m.signal()
	return j, nil
}

// Cancel はジョブを取り消す。待ちならそのまま、実行中ならコピーを止める。
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	switch e.job.Status {
	case StatusQueued:
		e.job.Status = StatusCanceled
		e.job.ErrorCode = CodeCanceled
		e.job.FinishedAt = m.now()
		m.persist(&e.job)
		j := e.job
		m.pruneLocked()
		m.mu.Unlock()
		m.notifyQueue()
		m.finished(j, 0)
		return nil
	case StatusRunning:
		cancel := e.cancel
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	default:
		m.mu.Unlock()
		return ErrState
	}
}

// Retry は失敗か取り消しのジョブを、同じ内容の新しいジョブとして入れ直す。
func (m *Manager) Retry(id string) (Job, error) {
	m.mu.Lock()
	e, ok := m.entries[id]
	if !ok {
		m.mu.Unlock()
		return Job{}, ErrNotFound
	}
	if e.job.Status != StatusFailed && e.job.Status != StatusCanceled {
		m.mu.Unlock()
		return Job{}, ErrState
	}
	j := e.job
	m.mu.Unlock()
	return m.enqueue(j.Source, j.Rel, j.Library, j.ID)
}

// Get はジョブを返す。
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[id]
	if !ok {
		return Job{}, false
	}
	return m.snapshotLocked(e), true
}

// List はジョブを新しい順に返す。
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		out = append(out, m.snapshotLocked(m.entries[m.order[i]]))
	}
	return out
}

// ActiveKey は待ちか実行中のジョブを探すためのキー。
type ActiveKey struct{ Library, Rel string }

// Active は、待ちか実行中のジョブの状態を library と相対パスで返す。
func (m *Manager) Active() map[ActiveKey]Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[ActiveKey]Job{}
	for _, e := range m.entries {
		if !e.job.Finished() {
			out[ActiveKey{e.job.Library, e.job.Rel}] = m.snapshotLocked(e)
		}
	}
	return out
}

// Counts は状態ごとのジョブの数を返す。
func (m *Manager) Counts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, e := range m.entries {
		out[e.job.Status]++
	}
	return out
}

func (m *Manager) snapshotLocked(e *entry) Job {
	j := e.job
	if j.Status == StatusRunning {
		j.BytesDone = e.bytesDone.Load()
		j.FilesDone = int(e.filesDone.Load())
	}
	return j
}

func (m *Manager) queuedLocked() int {
	n := 0
	for _, e := range m.entries {
		if e.job.Status == StatusQueued {
			n++
		}
	}
	return n
}

func (m *Manager) notifyQueue() {
	if m.hooks.OnQueueChange == nil {
		return
	}
	m.mu.Lock()
	n := m.queuedLocked()
	m.mu.Unlock()
	m.hooks.OnQueueChange(n)
}

func (m *Manager) persist(j *Job) {
	if m.store == nil {
		return
	}
	if err := m.store.Save(j); err != nil {
		m.log.Error("failed to save job history", "job", j.ID, "error", err)
	}
}

// pruneLocked は、終わったジョブが HistoryLimit を超えたら古いものから消す。
func (m *Manager) pruneLocked() {
	finished := 0
	for _, id := range m.order {
		if m.entries[id].job.Finished() {
			finished++
		}
	}
	excess := finished - m.opt.HistoryLimit
	if excess <= 0 {
		return
	}
	var removed []string
	kept := m.order[:0]
	for _, id := range m.order {
		if excess > 0 && m.entries[id].job.Finished() {
			removed = append(removed, id)
			delete(m.entries, id)
			excess--
			continue
		}
		kept = append(kept, id)
	}
	m.order = kept
	if m.store != nil {
		if err := m.store.Delete(removed); err != nil {
			m.log.Error("failed to prune job history", "error", err)
		}
	}
}

func (m *Manager) next(ctx context.Context) (*entry, context.Context, bool) {
	for {
		m.mu.Lock()
		var picked *entry
		for _, id := range m.order {
			e := m.entries[id]
			if e.job.Status == StatusQueued {
				picked = e
				break
			}
		}
		if picked != nil {
			jctx, cancel := context.WithCancel(ctx)
			picked.cancel = cancel
			picked.job.Status = StatusRunning
			picked.job.StartedAt = m.now()
			picked.bytesDone.Store(0)
			picked.filesDone.Store(0)
			m.persist(&picked.job)
			m.mu.Unlock()
			m.notifyQueue()
			// 他のワーカーにも次を探させる。
			m.signal()
			return picked, jctx, true
		}
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, false
		case <-m.wake:
		}
	}
}

func (m *Manager) worker(ctx context.Context) {
	defer m.wg.Done()
	for {
		e, jctx, ok := m.next(ctx)
		if !ok {
			return
		}
		m.execute(ctx, jctx, e)
	}
}

func (m *Manager) execute(parent, ctx context.Context, e *entry) {
	m.mu.Lock()
	j := e.job
	m.mu.Unlock()
	start := m.now()
	log := m.log.With("job", j.ID, "source", j.Source, "path", j.Rel, "library", j.Library)
	log.Info("job started")

	res, err := m.copyAlbum(ctx, &j, e)

	m.mu.Lock()
	e.cancel = nil
	e.job.BytesTotal = j.BytesTotal
	e.job.FilesTotal = j.FilesTotal
	e.job.BytesDone = e.bytesDone.Load()
	e.job.FilesDone = int(e.filesDone.Load())
	e.job.FinishedAt = m.now()
	switch {
	case err == nil:
		e.job.Status = StatusCompleted
		e.job.Resync = res.resync
	case ctx.Err() != nil && parent.Err() == nil:
		e.job.Status = StatusCanceled
		e.job.ErrorCode = CodeCanceled
	case parent.Err() != nil:
		// プロセスが止まるところ。次の起動で interrupted にする代わりに、ここで記録する。
		e.job.Status = StatusFailed
		e.job.ErrorCode = CodeInterrupted
		e.job.ErrorMessage = "the process is shutting down"
	default:
		e.job.Status = StatusFailed
		e.job.ErrorCode, e.job.ErrorMessage = classify(err)
	}
	done := e.job
	m.persist(&e.job)
	m.pruneLocked()
	m.mu.Unlock()

	dur := done.FinishedAt.Sub(start)
	attrs := []any{"bytes", done.BytesDone, "files", done.FilesDone, "duration_ms", dur.Milliseconds()}
	switch done.Status {
	case StatusCompleted:
		log.Info("job completed", append(attrs, "resync", done.Resync)...)
	case StatusCanceled:
		log.Info("job canceled", attrs...)
	default:
		log.Error("job failed", append(attrs, "error_code", done.ErrorCode, "error", done.ErrorMessage)...)
	}
	m.finished(done, dur)
}

func (m *Manager) finished(j Job, d time.Duration) {
	if m.hooks.OnFinished != nil {
		m.hooks.OnFinished(j, d)
	}
}

// SortByCreated は新しい順に並べる。
func SortByCreated(js []Job) {
	sort.Slice(js, func(i, k int) bool { return js[i].CreatedAt.After(js[k].CreatedAt) })
}
