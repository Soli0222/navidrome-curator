// Package metrics は Prometheus のメトリクスを持つ。接頭辞は navidrome_curator_。
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "navidrome_curator"

// Metrics はアプリのメトリクス。
type Metrics struct {
	Registry *prometheus.Registry

	JobsTotal        *prometheus.CounterVec
	CopiedBytesTotal prometheus.Counter
	QueueLength      prometheus.Gauge
	LibraryBytes     *prometheus.GaugeVec
	LibraryAlbums    *prometheus.GaugeVec
	FilesystemFree   prometheus.Gauge
	TrashBytes       prometheus.Gauge
	SourceUp         *prometheus.GaugeVec
	IndexLastSuccess *prometheus.GaugeVec
	NotifyFailures   prometheus.Counter
}

// New はメトリクスを作り、登録する。
func New() *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{
		Registry: r,
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "jobs_total", Help: "Finished copy jobs by result.",
		}, []string{"result"}),
		CopiedBytesTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "copied_bytes_total", Help: "Bytes copied into staging.",
		}),
		QueueLength: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "queue_length", Help: "Jobs waiting to run.",
		}),
		LibraryBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "library_bytes", Help: "Total size of the albums in each library.",
		}, []string{"library"}),
		LibraryAlbums: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "library_albums", Help: "Number of albums in each library.",
		}, []string{"library"}),
		FilesystemFree: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "filesystem_free_bytes", Help: "Free bytes on the library root filesystem.",
		}),
		TrashBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "trash_bytes", Help: "Bytes held in the trash.",
		}),
		SourceUp: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "source_up", Help: "Whether the latest index build or read of the source succeeded.",
		}, []string{"source"}),
		IndexLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "index_last_success_timestamp_seconds", Help: "Time of the latest successful index build.",
		}, []string{"source"}),
		NotifyFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "notify_failures_total", Help: "Failed webhook notifications.",
		}),
	}
	r.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.JobsTotal, m.CopiedBytesTotal, m.QueueLength, m.LibraryBytes, m.LibraryAlbums,
		m.FilesystemFree, m.TrashBytes, m.SourceUp, m.IndexLastSuccess, m.NotifyFailures,
	)
	for _, res := range []string{"completed", "failed", "canceled"} {
		m.JobsTotal.WithLabelValues(res)
	}
	return m
}
