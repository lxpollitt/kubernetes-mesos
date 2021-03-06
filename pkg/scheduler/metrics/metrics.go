package metrics

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	schedulerSubsystem = "scheduler"
)

var (
	QueueWaitTime = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Subsystem: schedulerSubsystem,
			Name:      "queue_wait_time_microseconds",
			Help:      "Launch queue wait time in microseconds",
		},
	)
	BindLatency = prometheus.NewSummary(
		prometheus.SummaryOpts{
			Subsystem: schedulerSubsystem,
			Name:      "bind_latency_microseconds",
			Help:      "Latency in microseconds between pod-task launch and pod binding.",
		},
	)
)

var registerMetrics sync.Once

func Register() {
	registerMetrics.Do(func() {
		prometheus.MustRegister(QueueWaitTime)
		prometheus.MustRegister(BindLatency)
	})
}

func InMicroseconds(d time.Duration) float64 {
	return float64(d.Nanoseconds() / time.Microsecond.Nanoseconds())
}
