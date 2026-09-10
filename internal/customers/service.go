// Package customers owns the customer bounded context: identity across
// channels via multiple identifiers, normalization, tags. Tables:
// customers, customer_identifiers, customer_tags.
package customers

import (
        "context"
        "errors"
        "regexp"
        "strings"
        "time"

        "github.com/google/uuid"
        "github.com/jackc/pgx/v5"
        "github.com/jackc/pgx/v5/pgconn"
        "github.com/jackc/pgx/v5/pgxpool"

        apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
        "github.com/Roy-Wanyoike/orvexa/pkg/pagination"
)

// Identifier types (mirrors migration 0002).
const (
        IdentPhone       = "phone"
        IdentWhatsApp    = "whatsapp"
        IdentEmail       = "email"
        IdentExternalCRM = "external_crm"
        IdentHandle      = "handle"
)

// Customer is the aggregate root.
type Customer struct {
        ID          string            `json:"id"`
        TenantID    string            `json:"tenant_id"`
        DisplayName string            `json:"display_name,omitempty"`
        Company     string            `json:"company,omitempty"`
        Language    string            `json:"language"`
        Timezone    string            `json:"timezone"`
        Attributes  map[string]any    `json:"attributes"`
        Tags        []string          `json:"tags,omitempty"`
        Identifiers []Identifier      `json:"identifiers,omitempty"`
        CreatedAt   time.Time         `json:"created_at"`
        UpdatedAt   time.Time         `json:"updated_at"`
}

// Identifier links one channel identity to a customer.
type Identifier struct {
        ID         string     `json:"id"`
        Type       string     `json:"type"`
        Value      string     `json:"value"`
        IsPrimary  bool       `json:"is_primary"`
        VerifiedAt *time.Time `json:"verified_at,omitempty"`
        CreatedAt  time.Time  `json:"created_at"`
}

// CreateInput is the validated write model.
type CreateInput struct {
        DisplayName string           `json:"display_name"`
        Company     string           `json:"company"`
        Language    string           `json:"language"`
        Timezone    string           `json:"timezone"`
        Attributes  map[string]any   `json:"attributes"`
        Identifiers []IdentifierIn   `json:"identifiers"`
        Tags        []string         `json:"tags"`
}

type IdentifierIn struct {
        Type      string `json:"type"`
        Value     string `json:"value"`
        IsPrimary bool   `json:"is_primary"`
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
var phoneRe = regexp.MustCompile(`^\+?[0-9]{7,15}$`)

// NormalizeIdentifier canonicalizes (type, value) so channel identity resolves
// to exactly one customer. Phone/WhatsApp → E.164 digits with '+'; email →
// lowercase trim. Returns an error for values that cannot be validated.
func NormalizeIdentifier(idType, value string) (string, error) {
        v := strings.TrimSpace(value)
        switch idType {
        case IdentPhone, IdentWhatsApp:
                v = strings.ReplaceAll(v, " ", "")
                v = strings.TrimPrefix(v, "00")
                if !strings.HasPrefix(v, "+") {
                        v = "+" + v
                }
                if !phoneRe.MatchString(v) {
                        return "", apperrors.Invalid("customer.invalid_identifier", "phone value must be 7-15 digits")
                }
                return v, nil
        case IdentEmail:
                v = strings.ToLower(v)
                if !emailRe.MatchString(v) {
                        return "", apperrors.Invalid("customer.invalid_identifier", "invalid email address")
                }
                return v, nil
        case IdentExternalCRM, IdentHandle:
                if v == "" || len(v) > 256 {
                        return "", apperrors.Invalid("customer.invalid_identifier", "identifier value must be 1-256 chars")
                }
                return v, nil
        default:
                return "", apperrors.Invalid("customer.invalid_identifier_type",
                        "type must be phone|whatsapp|email|external_crm|handle")
        }
}

// Service implements the customer use-cases.
type Service struct {
        pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Create persists a customer with identifiers and tags in one transaction.
// Identifier uniqueness conflicts map to 409, not 500.
func (s *Service) Create(ctx context.Context, tenantID string, in CreateInput) (*Customer, error) {
        if len(in.Identifiers) == 0 {
                return nil, apperrors.Invalid("customer.identifiers_required",
                        "at least one identifier is required")
        }
        // normalize up-front so validation errors surface before any write
        norm := make([]IdentifierIn, 0, len(in.Identifiers))
        seen := map[string]bool{}
        for _, idf := range in.Identifiers {
                val, err := NormalizeIdentifier(idf.Type, idf.Value)
                if err != nil {
                        return nil, err
                }
                k := idf.Type + ":" + val
                if seen[k] {
                        return nil, apperrors.Invalid("customer.duplicate_identifier", "identifier repeated in request")
                }
                seen[k] = true
                norm = append(norm, IdentifierIn{Type: idf.Type, Value: val, IsPrimary: idf.IsPrimary})
        }

        c := &Customer{
                ID:         uuid.NewString(),
                TenantID:   tenantID,
                Language:   defaultStr(in.Language, "en"),
                Timezone:   defaultStr(in.Timezone, "Africa/Nairobi"),
                Attributes: in.Attributes,
                Company:    in.Company,
                DisplayName: in.DisplayName,
                CreatedAt:  time.Now().UTC(),
                UpdatedAt:  time.Now().UTC(),
        }
        if c.Attributes == nil {
                c.Attributes = map[string]any{}
        }

        tx, err := s.pool.Begin(ctx)
        if err != nil {
                return nil, apperrors.Internal("db.tx_failed", "write failed").WithCause(err)
        }
        defer tx.Rollback(context.Background())

        _, err = tx.Exec(ctx, `
                INSERT INTO customers (id, tenant_id, display_name, company, language, timezone, attributes)
                VALUES ($1,$2,$3,$4,$5,$6,$7)`,
                c.ID, tenantID, c.DisplayName, c.Company, c.Language, c.Timezone, c.Attributes)
        if isUniqueViolation(err) {
                return nil, apperrors.Conflict("customer.conflict", "customer already exists")
        }
        if err != nil {
                return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
        }

        for _, idf := range norm {
                _, err = tx.Exec(ctx, `
                        INSERT INTO customer_identifiers (id, tenant_id, customer_id, type, value, is_primary)
                        VALUES ($1,$2,$3,$4,$5,$6)`,
                        uuid.NewString(), tenantID, c.ID, idf.Type, idf.Value, idf.IsPrimary)
                if isUniqueViolation(err) {
                        return nil, apperrors.Conflict("customer.identifier_exists",
                                "identifier already belongs to another customer")
                }
                if err != nil {
                        return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
                }
        }
        for _, tag := range dedupe(in.Tags) {
                if tag == "" || len(tag) > 64 {
                        return nil, apperrors.Invalid("customer.invalid_tag", "tags must be 1-64 chars")
                }
                _, err = tx.Exec(ctx, `
                        INSERT INTO customer_tags (tenant_id, customer_id, tag) VALUES ($1,$2,$3)`,
                        tenantID, c.ID, tag)
                if err != nil {
                        return nil, apperrors.Internal("db.write_failed", "write failed").WithCause(err)
                }
        }

        if err := tx.Commit(ctx); err != nil {
                return nil, apperrors.Internal("db.commit_failed", "write failed").WithCause(err)
        }
        return s.Get(ctx, tenantID, c.ID)
}

// Get returns one tenant-scoped customer (foreign ids read as not-found).
func (s *Service) Get(ctx context.Context, tenantID, id string) (*Customer, error) {
        row := s.pool.QueryRow(ctx, `
                SELECT id, tenant_id, coalesce(display_name,''), coalesce(company,''), language, timezone, attributes, created_at, updated_at
                FROM customers WHERE id = $1 AND tenant_id = $2`, id, tenantID)
        c := &Customer{}
        var attrs map[string]any
        if err := row.Scan(&c.ID, &c.TenantID, &c.DisplayName, &c.Company, &c.Language,
                &c.Timezone, &attrs, &c.CreatedAt, &c.UpdatedAt); err != nil {
                if err == pgx.ErrNoRows {
                        return nil, apperrors.NotFound("customer.not_found", "customer not found")
                }
                return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        c.Attributes = attrs
        if err := s.attachRelations(ctx, tenantID, c); err != nil {
                return nil, err
        }
        return c, nil
}

// Resolve maps a channel identifier to the customer — the backbone that makes
// "phone → customer → conversation history" work regardless of channel.
func (s *Service) Resolve(ctx context.Context, tenantID, idType, value string) (*Customer, error) {
        val, err := NormalizeIdentifier(idType, value)
        if err != nil {
                return nil, err
        }
        row := s.pool.QueryRow(ctx, `
                SELECT c.id FROM customer_identifiers ci
                JOIN customers c ON c.id = ci.customer_id
                WHERE ci.tenant_id = $1 AND ci.type = $2 AND ci.value = $3`,
                tenantID, idType, val)
        var id string
        if err := row.Scan(&id); err != nil {
                if err == pgx.ErrNoRows {
                        return nil, apperrors.NotFound("customer.not_found", "no customer for identifier")
                }
                return nil, apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        return s.Get(ctx, tenantID, id)
}

// List returns a bounded page of tenant customers, newest first.
func (s *Service) List(ctx context.Context, tenantID string, page pagination.Page) ([]Customer, string, error) {
        rows, err := s.pool.Query(ctx, `
                SELECT id, coalesce(display_name,''), coalesce(company,''), language, timezone, created_at, updated_at
                FROM customers WHERE tenant_id = $1
                ORDER BY created_at DESC, id
                LIMIT $2 OFFSET $3`, tenantID, page.Limit+1, page.Offset)
        if err != nil {
                return nil, "", apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        defer rows.Close()

        out := make([]Customer, 0, page.Limit)
        for rows.Next() {
                var c Customer
                if err := rows.Scan(&c.ID, &c.DisplayName, &c.Company, &c.Language, &c.Timezone,
                        &c.CreatedAt, &c.UpdatedAt); err != nil {
                        return nil, "", apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
                }
                out = append(out, c)
        }
        hasMore := len(out) > page.Limit
        if hasMore {
                out = out[:page.Limit]
        }
        cursor := ""
        if hasMore {
                cursor = page.NextCursor(page.Limit)
        }
        return out, cursor, nil
}

func (s *Service) attachRelations(ctx context.Context, tenantID string, c *Customer) error {
        idRows, err := s.pool.Query(ctx, `
                SELECT id, type, value, is_primary, verified_at, created_at
                FROM customer_identifiers WHERE customer_id = $1 AND tenant_id = $2
                ORDER BY is_primary DESC, created_at`, c.ID, tenantID)
        if err != nil {
                return apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        defer idRows.Close()
        c.Identifiers = []Identifier{}
        for idRows.Next() {
                var i Identifier
                if err := idRows.Scan(&i.ID, &i.Type, &i.Value, &i.IsPrimary, &i.VerifiedAt, &i.CreatedAt); err != nil {
                        return apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
                }
                c.Identifiers = append(c.Identifiers, i)
        }

        tagRows, err := s.pool.Query(ctx, `
                SELECT tag FROM customer_tags WHERE customer_id = $1 AND tenant_id = $2 ORDER BY tag`, c.ID, tenantID)
        if err != nil {
                return apperrors.Internal("db.read_failed", "read failed").WithCause(err)
        }
        defer tagRows.Close()
        c.Tags = []string{}
        for tagRows.Next() {
                var t string
                if err := tagRows.Scan(&t); err != nil {
                        return apperrors.Internal("db.scan_failed", "read failed").WithCause(err)
                }
                c.Tags = append(c.Tags, t)
        }
        return nil
}

func isUniqueViolation(err error) bool {
        var pgErr *pgconn.PgError
        return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func defaultStr(v, def string) string {
        if strings.TrimSpace(v) == "" {
                return def
        }
        return v
}

func dedupe(in []string) []string {
        seen := map[string]bool{}
        out := make([]string, 0, len(in))
        for _, s := range in {
                if !seen[s] {
                        seen[s] = true
                        out = append(out, s)
                }
        }
        return out
}
