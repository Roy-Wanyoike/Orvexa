// Package dbinternal holds small cross-domain database helpers that don't
// justify their own import cycles.
package dbinternal

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsUniqueViolation reports PostgreSQL unique-constraint violations (23505).
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
