package interactions

import (
        "context"
        "errors"
        "time"

        "github.com/google/uuid"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgconn"
        "github.com/jackc/pgx/v5/pgxpool"

        apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
        "github.com/Roy-Wanyoike/orvexa/pkg/events"
        "github.com/Roy-Wanyoike/orvexa/pkg/idempotency"
        "github.com/Roy-Wanyoike/orvexa/pkg/pagination"
)

// Outbox is the port the interaction module uses to persist domain events
// atomically with state (transactional outbox pattern — see platform/outbox).
type Outbox interface {
        Insert(ctx context.Context, tx pgx.Tx, env *events.Envelope) error
}

// Service implements interaction use-cases on top of the conversation model.
type Service struct {
        pool   *pgxpool.Pool
        outbox Outbox
        source string
}

func NewService(pool *pgxpool.Pool, ob Outbox, source string) *Service {
        return &Service{pool: pool, outbox: ob, source: source}
}

// CreateInput is the validated create model. Conversation handling:
// conversationID empty + inbound → find-or-open the customer's open
// conversation for the channel (continuous context across interactions).
type CreateInput struct {
        ConversationID string
        CustomerID     string
        Channel        Channel
        Direction      Direction
        Source         string
        Destination    string
        Provider       string
        ProviderRef    string
        IdempotencyKey string
        Attributes     map[string]any
}

// Create opens an interaction. Externally-triggered creates MUST carry an
// idempotency key (provider event id) so replays are single-effect.
func (s *Service) Create(ctx context.Context, tenantID string, in CreateInput) (*Rec, error) {
        if in.CustomerID == "" {
                return nil, apperrors.Invalid("interaction.customer_required", "customer_id is required")
        }
        switch in.Channel {
        case ChannelVoice, ChannelWhatsApp, ChannelSMS, ChannelEmail, ChannelChat, ChannelUSSD:
        default:
                return nil, apperrors.Invalid("interaction.invalid_channel", "unknown channel")
        }
        if in.Direction != DirectionInbound && in.Direction != DirectionOutbound {
                return nil, apperrors.Invalid("interaction.invalid_direction", "direction must be inbound|outbound")
        }
        if in.Source == "" || in.Destination == "" {
                return nil, apperrors.Invalid("interaction.endpoints_required", "source and destination are required")
        }
        if in.IdempotencyKey == "" && in.ProviderRef != "" {
                in.IdempotencyKey = idempotency.Derive(in.Provider, in.ProviderRef)
        }
        if in.IdempotencyKey != "" {
                if err := idempotency.Validate(in.IdempotencyKey); err != nil {
                        return nil, apperrors.Invalid("interaction.invalid_idempotency_key", err.Error())
                }
        }
        provider := defaultStr(in.Provider, "internal")

        rec := &Rec{
                ID:             uuid.NewString(),
                TenantID:       tenantID,
                Channel:        in.Channel,
                Direction:      in.Direction,
                Status:         StatusPending,
                Source:         in.Source,
                Destination:    in.Destination,
                Provider:       provider,
                ProviderRef:    in.ProviderRef,
                IdempotencyKey: in.IdempotencyKey,
                Attributes:     in.Attributes,
                StartedAt:      time.Now().UTC(),
        }
        if rec.Attributes == nil {
                rec.Attributes = map[string]any{}
        }

        tx, err := s.pool.Begin(ctx)
        if err != nil {
                return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
        }
        defer tx.Rollback(context.Background())

        // resolve or open the conversation inside the tx for consistency
        convID := in.ConversationID
        if convID == "" {
                convID, err = s.findOrOpenConversation(ctx, tx, tenantID, in)
                if err != nil {
                        return nil, err
                }
        } else {
                var custID, status string
                err = tx.QueryRow(ctx, `SELECT customer_id, status FROM conversations
                        WHERE id = $1 AND tenant_id = $2 FOR SHARE`, convID, tenantID).Scan(&custID, &status)
                if err == pgx.ErrNoRows {
                        return nil, apperrors.NotFound("conversation.not_found", "conversation not found")
                }
                if err != nil {
                        return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
                }
                if status != "open" {
                        return nil, apperrors.Conflict("conversation.closed", "conversation is closed")
                }
                if custID != in.CustomerID {
                        return nil, apperrors.Conflict("conversation.customer_mismatch",
                                "customer does not belong to this conversation")
                }
        }
        rec.ConversationID = convID
        rec.CustomerID = in.CustomerID

        _, err = tx.Exec(ctx, `
                INSERT INTO interactions
                        (id, tenant_id, conversation_id, customer_id, channel, direction, status,
                         source, destination, provider, provider_ref, idempotency_key, attributes, started_at)
                VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
                rec.ID, tenantID, rec.ConversationID, rec.CustomerID, rec.Channel, rec.Direction,
                rec.Status, rec.Source, rec.Destination, rec.Provider, rec.ProviderRef,
                nullStr(rec.IdempotencyKey), rec.Attributes, rec.StartedAt)
        if isUnique(err) {
                // duplicate delivery: return the existing interaction (idempotent)
                if existing := s.findByProviderRef(ctx, tenantID, provider, in.ProviderRef); existing != nil {
                        return existing, nil
                }
                return nil, apperrors.Conflict("interaction.duplicate", "interaction already exists")
        }
        if err != nil {
                return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
        }

        // touch conversation + emit events atomically with the write
        _, err = tx.Exec(ctx, `
                UPDATE conversations SET last_interaction_at = now(), updated_at = now(), unread_count = unread_count + 1
                WHERE id = $1 AND tenant_id = $2`, rec.ConversationID, tenantID)
        if err != nil {
                return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
        }

        env, _ := events.New(events.TopicInteractionCreated, s.source, rec.ID, tenantID, "", map[string]any{
                "interaction_id":  rec.ID,
                "conversation_id": rec.ConversationID,
                "customer_id":     rec.CustomerID,
                "channel":         rec.Channel,
                "direction":       rec.Direction,
                "status":          rec.Status,
        })
        if err := s.outbox.Insert(ctx, tx, env); err != nil {
                return nil, apperrors.Internal("outbox.insert_failed", "write failed").WithCause(err)
        }
        if err := tx.Commit(ctx); err != nil {
                return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
        }
        return rec, nil
}

// Transition moves an interaction through guarded lifecycle states and
// appends the domain event transactionally. Illegal moves are 409.
func (s *Service) Transition(ctx context.Context, tenantID, id string, to Status, endReason string) (*Rec, error) {
        tx, err := s.pool.Begin(ctx)
        if err != nil {
                return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
        }
        defer tx.Rollback(context.Background())

        row := tx.QueryRow(ctx, `SELECT conversation_id, customer_id, channel, direction, status,
                source, destination, provider, coalesce(provider_ref,''), attributes
                FROM interactions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID)
        cur := &Rec{ID: id, TenantID: tenantID}
        var status string
        if err := row.Scan(&cur.ConversationID, &cur.CustomerID, &cur.Channel, &cur.Direction,
                &status, &cur.Source, &cur.Destination, &cur.Provider, &cur.ProviderRef,
                &cur.Attributes); err != nil {
                if err == pgx.ErrNoRows {
                        return nil, apperrors.NotFound("interaction.not_found", "interaction not found")
                }
                return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        cur.Status = Status(status)

        if err := Transition(cur.Status, to); err != nil {
                return nil, err
        }

        now := time.Now().UTC()
        var answeredAt *time.Time
        if to == StatusActive {
                answeredAt = &now
        }
        var endedAt *time.Time
        if to.Terminal() {
                endedAt = &now
                cur.EndReason = endReason
        }

        _, err = tx.Exec(ctx, `
                UPDATE interactions SET status = $3,
                        answered_at = coalesce($4, answered_at),
                        ended_at = $5, end_reason = coalesce($6, end_reason), updated_at = now()
                WHERE id = $1 AND tenant_id = $2`, id, tenantID, to, answeredAt, endedAt, nullStr(cur.EndReason))
        if err != nil {
                return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
        }

        topic := events.TopicInteractionUpdated
        if to == StatusCompleted {
                topic = events.TopicInteractionCompleted
        }
        env, _ := events.New(topic, s.source, id, tenantID, "", map[string]any{
                "interaction_id": id, "from": string(cur.Status), "to": string(to),
                "end_reason": cur.EndReason,
        })
        if err := s.outbox.Insert(ctx, tx, env); err != nil {
                return nil, apperrors.Internal("outbox.insert_failed", "write failed").WithCause(err)
        }
        if err := tx.Commit(ctx); err != nil {
                return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
        }
        return s.Get(ctx, tenantID, id)
}

// Assign records the agent/queue owning the interaction.
func (s *Service) Assign(ctx context.Context, tenantID, id, agentID, queueID string) (*Rec, error) {
        if agentID == "" && queueID == "" {
                return nil, apperrors.Invalid("interaction.assign_target_required", "agent_id or queue_id required")
        }
        tag := "interaction.updated"
        res, err := s.pool.Exec(ctx, `
                UPDATE interactions SET
                        assigned_agent_id = coalesce($3, assigned_agent_id),
                        assigned_queue_id = coalesce($4, assigned_queue_id),
                        updated_at = now()
                WHERE id = $1 AND tenant_id = $2 AND status IN ('pending','active')`,
                id, tenantID, nullStr(agentID), nullStr(queueID))
        if err != nil {
                return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
        }
        if res.RowsAffected() == 0 {
                if _, gerr := s.Get(ctx, tenantID, id); gerr != nil {
                        return nil, gerr
                }
                return nil, apperrors.Conflict("interaction.not_assignable",
                        "interaction is not in an assignable state")
        }
        s.emit(ctx, tag, id, tenantID, map[string]any{
                "interaction_id": id, "agent_id": agentID, "queue_id": queueID,
        })
        return s.Get(ctx, tenantID, id)
}

// Get returns one tenant-scoped interaction.
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Rec, error) {
        row := s.pool.QueryRow(ctx, `
                SELECT id, tenant_id, conversation_id, customer_id, channel, direction, status,
                        source, destination, coalesce(assigned_agent_id::text,''), coalesce(assigned_queue_id::text,''),
                        started_at, answered_at, ended_at, coalesce(end_reason,''), provider, coalesce(provider_ref,''),
                        coalesce(idempotency_key,''), attributes
                FROM interactions WHERE id = $1 AND tenant_id = $2`, id, tenantID)
        rec := &Rec{}
        var channel, direction, status string
        if err := row.Scan(&rec.ID, &rec.TenantID, &rec.ConversationID, &rec.CustomerID,
                &channel, &direction, &status, &rec.Source, &rec.Destination,
                &rec.AssignedAgentID, &rec.AssignedQueueID, &rec.StartedAt, &rec.AnsweredAt,
                &rec.EndedAt, &rec.EndReason, &rec.Provider, &rec.ProviderRef,
                &rec.IdempotencyKey, &rec.Attributes); err != nil {
                if err == pgx.ErrNoRows {
                        return nil, apperrors.NotFound("interaction.not_found", "interaction not found")
                }
                return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        rec.Channel, rec.Direction, rec.Status = Channel(channel), Direction(direction), Status(status)
        return rec, nil
}

// ListByConversation returns the interaction timeline of a conversation.
func (s *Service) ListByConversation(ctx context.Context, tenantID, conversationID string, page pagination.Page) ([]Rec, string, error) {
        rows, err := s.pool.Query(ctx, `
                SELECT id, channel, direction, status, source, destination,
                        coalesce(assigned_agent_id::text,''), coalesce(assigned_queue_id::text,''),
                        started_at, answered_at, ended_at, coalesce(end_reason,''), provider, coalesce(provider_ref,'')
                FROM interactions
                WHERE tenant_id = $1 AND conversation_id = $2
                ORDER BY started_at DESC, id
                LIMIT $3 OFFSET $4`, tenantID, conversationID, page.Limit+1, page.Offset)
        if err != nil {
                return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        defer rows.Close()
        out := []Rec{}
        for rows.Next() {
                var r Rec
                var channel, direction, status string
                if err := rows.Scan(&r.ID, &channel, &direction, &status, &r.Source, &r.Destination,
                        &r.AssignedAgentID, &r.AssignedQueueID, &r.StartedAt, &r.AnsweredAt, &r.EndedAt,
                        &r.EndReason, &r.Provider, &r.ProviderRef); err != nil {
                        return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
                }
                r.Channel, r.Direction, r.Status = Channel(channel), Direction(direction), Status(status)
                out = append(out, r)
        }
        hasMore := len(out) > page.Limit
        if hasMore {
                out = out[:page.Limit]
        }
        var cursor string
        if hasMore {
                cursor = page.NextCursor(page.Limit)
        }
        return out, cursor, nil
}

// findOrOpenConversation reuses the customer's open conversation for the
// channel (or opens one), preserving one continuous context.
func (s *Service) findOrOpenConversation(ctx context.Context, tx pgx.Tx, tenantID string, in CreateInput) (string, error) {
        var convID string
        err := tx.QueryRow(ctx, `
                SELECT id FROM conversations
                WHERE tenant_id = $1 AND customer_id = $2 AND channel = $3 AND status = 'open'
                ORDER BY updated_at DESC LIMIT 1 FOR UPDATE`,
                tenantID, in.CustomerID, in.Channel).Scan(&convID)
        if err == nil {
                return convID, nil
        }
        if err != pgx.ErrNoRows {
                return "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        convID = uuid.NewString()
        _, err = tx.Exec(ctx, `
                INSERT INTO conversations (id, tenant_id, customer_id, channel)
                VALUES ($1,$2,$3,$4)`, convID, tenantID, in.CustomerID, in.Channel)
        if err != nil {
                return "", apperrors.Internal("db.write_failed", "write failed").WithCause(err)
        }
        _, err = tx.Exec(ctx, `
                INSERT INTO conversation_participants (conversation_id, participant_type, participant_id)
                VALUES ($1,'customer',$2)`, convID, in.CustomerID)
        if err != nil {
                return "", apperrors.Internal("db.write_failed", "write failed").WithCause(err)
        }
        env, _ := events.New(events.TopicConversationOpened, s.source, convID, tenantID, "", map[string]any{
                "conversation_id": convID, "customer_id": in.CustomerID, "channel": in.Channel,
        })
        if err := s.outbox.Insert(ctx, tx, env); err != nil {
                return "", apperrors.Internal("outbox.insert_failed", "write failed").WithCause(err)
        }
        return convID, nil
}

// findByProviderRef is the idempotent replay path for duplicate creates.
func (s *Service) findByProviderRef(ctx context.Context, tenantID, provider, ref string) *Rec {
        if ref == "" {
                return nil
        }
        row := s.pool.QueryRow(ctx, `SELECT id FROM interactions
                WHERE tenant_id = $1 AND provider = $2 AND provider_ref = $3`, tenantID, provider, ref)
        var id string
        if err := row.Scan(&id); err != nil {
                return nil
        }
        rec, err := s.Get(ctx, tenantID, id)
        if err != nil {
                return nil
        }
        return rec
}

// emit is a best-effort post-commit event for non-critical notices.
func (s *Service) emit(ctx context.Context, topic, subject, tenantID string, data map[string]any) {
        env, err := events.New(topic, s.source, subject, tenantID, "", data)
        if err != nil {
                return
        }
        tx, err := s.pool.Begin(ctx)
        if err != nil {
                return
        }
        defer tx.Rollback(context.Background())
        _ = s.outbox.Insert(ctx, tx, env)
        _ = tx.Commit(ctx)
}

func nullStr(s string) any {
        if s == "" {
                return nil
        }
        return s
}

func isUnique(err error) bool {
        var pgErr *pgconn.PgError
        return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func defaultStr(v, def string) string {
        if v == "" {
                return def
        }
        return v
}
