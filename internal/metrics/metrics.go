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

	// JobsRetried counts jobs the worker put back on the queue after a
	// retryable failure, by type. High values indicate a flaky dependency.
	JobsRetried = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_jobs_retried_total",
		Help: "Jobs requeued by the worker after a retryable failure, by type.",
	}, []string{"type"})

	// JobsDropped counts jobs abandoned by the worker, by type and reason
	// (max_retries_exceeded|permanent). These jobs will not run again.
	JobsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_jobs_dropped_total",
		Help: "Jobs abandoned by the worker (no retry), by type and reason.",
	}, []string{"type", "reason"})

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

	// AIRequestDuration observes LiteLLM request latency by kind, so AI latency
	// is not conflated with overall command latency.
	AIRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "discord_dnd_bot_ai_request_duration_seconds",
		Help:    "LiteLLM request latency in seconds, by kind.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	}, []string{"kind"})

	// AITokens counts tokens reported by LiteLLM by kind and token role
	// (prompt|completion), for cost/usage monitoring.
	AITokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_ai_tokens_total",
		Help: "Tokens reported by LiteLLM, by kind and role (prompt|completion).",
	}, []string{"kind", "role"})

	// QueueDepth reports the current number of jobs waiting in the queue. Set
	// periodically by the worker so HPA / dashboards can see backlog directly.
	QueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "discord_dnd_bot_queue_depth",
		Help: "Number of jobs currently waiting in the Redis queue.",
	})

	// ComponentErrors counts errors from infrastructure components by component
	// (db|redis|storage|litellm|discord) and a coarse operation label, so
	// dependency failures are visible in Prometheus rather than only in logs.
	ComponentErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_component_errors_total",
		Help: "Errors from infrastructure components, by component and operation.",
	}, []string{"component", "operation"})

	// PanicsRecovered counts panics caught by recovery middleware/wrappers,
	// labelled by where they were recovered (interaction|job|http|goroutine).
	PanicsRecovered = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_panics_recovered_total",
		Help: "Panics recovered, by site (interaction|job|http|goroutine).",
	}, []string{"site"})

	// HTTPRequests counts health/metrics server requests by path and status.
	HTTPRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_http_requests_total",
		Help: "Health/metrics HTTP requests, by path and status code.",
	}, []string{"path", "status"})

	// DBPoolConns reports pgx pool connection counts by state (total|acquired|idle).
	DBPoolConns = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "discord_dnd_bot_db_pool_connections",
		Help: "pgx pool connection counts, by state (total|acquired|idle).",
	}, []string{"state"})

	// StateProposalsCreated counts campaign-state proposals produced by the
	// post-session extraction step. A run that produces zero (nothing meaningful
	// changed) still succeeds; this tracks how much the extractor is surfacing.
	StateProposalsCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "discord_dnd_bot_state_proposals_created_total",
		Help: "Campaign-state proposals created by post-session extraction.",
	})

	// StateProposalsReviewed counts proposals decided by a DM, by outcome
	// (approved|rejected). Idempotent no-ops (double-clicks) are not counted.
	StateProposalsReviewed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_state_proposals_reviewed_total",
		Help: "Campaign-state proposals reviewed by a DM, by outcome (approved|rejected).",
	}, []string{"outcome"})

	// AIResponsesTruncated counts model replies that stopped at the token limit
	// (finish_reason=length) rather than finishing, by task (notes|extract) and
	// whether a continuation round recovered the full text. A rising "final"
	// count means the task's max-tokens budget is too small: notes get cut
	// mid-sentence and extraction JSON arrives unparseable.
	AIResponsesTruncated = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "discord_dnd_bot_ai_responses_truncated_total",
		Help: "Model replies cut off at the token limit, by task and resolution (continued|final).",
	}, []string{"task", "resolution"})
)

// ComponentError is a small convenience wrapper that increments ComponentErrors.
func ComponentError(component, operation string) {
	ComponentErrors.WithLabelValues(component, operation).Inc()
}
