// Package metrics defines the Prometheus metrics shared across services so that
// each independently-scaled deployment reports comparable signals.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// CommandsTotal counts slash-command invocations by name and outcome.
	CommandsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_commands_total",
		Help: "Total Discord commands handled, by command and status.",
	}, []string{"command", "status"})

	// CommandDuration observes command handling latency.
	CommandDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "discord_dnd_bot_command_duration_seconds",
		Help:    "Command handling latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"command"})

	// JobsEnqueued counts jobs pushed to the queue by type.
	JobsEnqueued = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_jobs_enqueued_total",
		Help: "Jobs enqueued, by type.",
	}, []string{"type"})

	// JobsProcessed counts jobs handled by the worker by type and outcome.
	JobsProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_jobs_processed_total",
		Help: "Jobs processed by the worker, by type and status.",
	}, []string{"type", "status"})

	// JobDuration observes job processing latency.
	JobDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "discord_dnd_bot_job_duration_seconds",
		Help:    "Job processing latency in seconds.",
		Buckets: []float64{1, 5, 15, 30, 60, 120, 300, 600},
	}, []string{"type"})

	// AIRequests counts LiteLLM calls by kind and outcome.
	AIRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_ai_requests_total",
		Help: "LiteLLM requests, by kind (chat|transcribe|image) and status.",
	}, []string{"kind", "status"})
)
