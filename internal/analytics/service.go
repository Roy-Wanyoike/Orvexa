// Package analytics owns the facts pipeline: an event consumer that appends
// tenant-scoped facts from the bus, and read APIs with fixed aggregations
// over bounded windows. Dashboards never touch transactional tables.
package analytics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
	"github.com/Roy-Wanyoike/orvexa/pkg/events"
)

// Consumer appends facts for selected topics. Idempotent by event id
// (PRIMARY KEY on event_id) — at-least-once delivery is safe.
type Consumer struct {
	pool *pgxpool.Pool
}

func NewConsumer(pool *pgxpool.Pool) *Consumer { return &Consumer{pool: pool} }

// Handle processes one envelope into the matching fact table.
func (c *Consumer) Handle(ctx context.Context, env *events.Envelope) error {
	switch env.Type {
	case events.TopicInteractionCreated, events.TopicInteractionUpdated, events.TopicInteractionCompleted:
		return c.interactionFact(ctx, env)
	case events.TopicUsageRecorded:
		return c.usageFact(ctx, env)
	default:
		return nil // not a fact topic
	}
}

func (c *Consumer) interactionFact(ctx context.Context, env *events.Envelope) error {
	channel, _ := env.Data["channel"].(string)
	direction, _ := env.Data["direction"].(string)
	status, _ := env.Data["status"].(string)
	if status == "" {
		status = "unknown"
	}
	subject := env.Subject
	// interaction.updated events carry the id inside data
	if subject == "" {
		if v, ok := env.Data["interaction_id"].(string); ok {
			subject = v
		}
	}
	if subject == "" {
		return nil
	}
	_, err := c.pool.Exec(ctx, `
		INSERT INTO interaction_fact (event_id, tenant_id, interaction_id, channel, direction, status, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (event_id) DO NOTHING`,
		env.ID, env.TenantID, subject, channel, direction, status, env.Time)
	if err != nil {
		return apperrors.Internal("analytics.write_failed", "fact append failed").WithCause(err)
	}
	return nil
}

func (c *Consumer) usageFact(ctx context.Context, env *events.Envelope) error {
	metric, _ := env.Data["metric"].(string)
	amount := int64(0)
	switch v := env.Data["output_tokens"].(type) {
	case float64:
		amount = int64(v)
	case int:
		amount = int64(v)
	}
	_, err := c.pool.Exec(ctx, `
		INSERT INTO usage_fact (event_id, tenant_id, metric, amount, occurred_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (event_id) DO NOTHING`,
		env.ID, env.TenantID, metric, amount, env.Time)
	if err != nil {
		return apperrors.Internal("analytics.write_failed", "fact append failed").WithCause(err)
	}
	return nil
}

// Service serves the read API.
type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Summary is the fixed-aggregation read model.
type Summary struct {
	Window struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
	InteractionsByChannel map[string]int64 `json:"interactions_by_channel"`
	InteractionsByStatus  map[string]int64 `json:"interactions_by_status"`
	TotalInteractions     int64            `json:"total_interactions"`
	AIEvents              int64            `json:"ai_events"`
	TokensUsed            int64            `json:"tokens_used"`
	Freshness             time.Time        `json:"freshness"`
}

// Summary aggregates facts over a bounded window (max 30 days).
func (s *Service) Summary(ctx context.Context, tenantID string, from, to time.Time) (*Summary, error) {
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if from.IsZero() {
		from = to.Add(-24 * time.Hour)
	}
	if to.Sub(from) > 30*24*time.Hour {
		return nil, apperrors.Invalid("analytics.window_too_large", "window must be at most 30 days")
	}
	if to.Before(from) {
		return nil, apperrors.Invalid("analytics.window_invalid", "from must precede to")
	}

	sum := &Summary{
		InteractionsByChannel: map[string]int64{},
		InteractionsByStatus:  map[string]int64{},
	}
	sum.Window.From, sum.Window.To = from, to

	rows, err := s.pool.Query(ctx, `
		SELECT channel, status, count(DISTINCT interaction_id) FROM interaction_fact
		WHERE tenant_id = $1 AND occurred_at BETWEEN $2 AND $3
		GROUP BY channel, status`, tenantID, from, to)
	if err != nil {
		return nil, apperrors.Internal("analytics.read_failed", "aggregation failed").WithCause(err)
	}
	defer rows.Close()
	for rows.Next() {
		var channel, status string
		var n int64
		if err := rows.Scan(&channel, &status, &n); err != nil {
			return nil, apperrors.Internal("analytics.scan_failed", "aggregation failed").WithCause(err)
		}
		sum.InteractionsByChannel[channel] += n
		sum.InteractionsByStatus[status] += n
		sum.TotalInteractions += n
	}

	err = s.pool.QueryRow(ctx, `
		SELECT count(*), coalesce(sum(amount),0) FROM usage_fact
		WHERE tenant_id = $1 AND occurred_at BETWEEN $2 AND $3`, tenantID, from, to).
		Scan(&sum.AIEvents, &sum.TokensUsed)
	if err != nil {
		return nil, apperrors.Internal("analytics.read_failed", "aggregation failed").WithCause(err)
	}
	sum.Freshness = time.Now().UTC()
	return sum, nil
}
