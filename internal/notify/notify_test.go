package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBatchesEventsWithinDebounce(t *testing.T) {
	var (
		mu       sync.Mutex
		payloads []Payload
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		payloads = append(payloads, p)
		mu.Unlock()
	}))
	defer srv.Close()
	var failures atomic.Int32
	n := New(context.Background(), srv.URL, 100*time.Millisecond, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)), func() { failures.Add(1) })
	for _, id := range []string{"a", "b", "c"} {
		n.Add(Event{JobID: id, Result: "completed"})
	}
	time.Sleep(300 * time.Millisecond)
	n.Add(Event{JobID: "d", Result: "failed"})
	n.Flush()

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 || len(payloads[0].Events) != 3 || len(payloads[1].Events) != 1 {
		t.Fatalf("payloads = %+v", payloads)
	}
	if failures.Load() != 0 {
		t.Fatal("unexpected failures")
	}
}

func TestFailureIsCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var failures atomic.Int32
	n := New(context.Background(), srv.URL, time.Hour, time.Second,
		slog.New(slog.NewTextHandler(io.Discard, nil)), func() { failures.Add(1) })
	n.Add(Event{JobID: "a"})
	n.Flush()
	if failures.Load() != 1 {
		t.Fatalf("failures = %d", failures.Load())
	}
}

func TestDisabledWithoutURL(t *testing.T) {
	n := New(context.Background(), "", time.Second, time.Second, nil, nil)
	n.Add(Event{JobID: "a"}) // nil でも呼べる
	n.Flush()
}
