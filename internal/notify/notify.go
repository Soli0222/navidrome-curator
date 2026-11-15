// Package notify は、ジョブが終わったことを設定した URL に HTTP で知らせる。
//
// 一定時間の間に終わったジョブは1回の通知にまとめる。通知の失敗はジョブの結果を変えない。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Event は通知の1件。
type Event struct {
	JobID      string    `json:"jobId"`
	Source     string    `json:"source"`
	Path       string    `json:"path"`
	Library    string    `json:"library"`
	Result     string    `json:"result"`
	Resync     bool      `json:"resync"`
	ErrorCode  string    `json:"errorCode,omitempty"`
	FinishedAt time.Time `json:"finishedAt"`
}

// Payload は通知の本文。
type Payload struct {
	Events []Event `json:"events"`
}

// Notifier は通知をまとめて送る。
type Notifier struct {
	url      string
	debounce time.Duration
	client   *http.Client
	log      *slog.Logger
	onFail   func()

	mu      sync.Mutex
	pending []Event
	timer   *time.Timer
	ctx     context.Context
	wg      sync.WaitGroup
}

// New は Notifier を作る。url が空なら nil を返す。
func New(ctx context.Context, url string, debounce, timeout time.Duration, log *slog.Logger, onFail func()) *Notifier {
	if url == "" {
		return nil
	}
	return &Notifier{
		url: url, debounce: debounce, log: log, onFail: onFail, ctx: ctx,
		client: &http.Client{Timeout: timeout},
	}
}

// Add は通知を1件足す。最初の1件から debounce が過ぎたら、たまったものをまとめて送る。
func (n *Notifier) Add(e Event) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pending = append(n.pending, e)
	if n.timer == nil {
		n.timer = time.AfterFunc(n.debounce, n.flush)
	}
}

// Flush はたまっている通知をすぐに送り、送り終わるのを待つ。
func (n *Notifier) Flush() {
	if n == nil {
		return
	}
	n.mu.Lock()
	if n.timer != nil {
		n.timer.Stop()
	}
	n.mu.Unlock()
	n.flush()
	n.wg.Wait()
}

func (n *Notifier) flush() {
	n.mu.Lock()
	events := n.pending
	n.pending = nil
	n.timer = nil
	n.mu.Unlock()
	if len(events) == 0 {
		return
	}
	n.wg.Add(1)
	defer n.wg.Done()
	if err := n.send(events); err != nil {
		n.log.Error("webhook notification failed", "events", len(events), "error", err)
		if n.onFail != nil {
			n.onFail()
		}
		return
	}
	n.log.Info("webhook notification sent", "events", len(events))
}

func (n *Notifier) send(events []Event) error {
	body, err := json.Marshal(Payload{Events: events})
	if err != nil {
		return err
	}
	// プロセスを止めるときにも送り切れるよう、親の ctx が終わっていてもタイムアウトまでは待つ。
	ctx := context.WithoutCancel(n.ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "navidrome-curator")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	return nil
}
