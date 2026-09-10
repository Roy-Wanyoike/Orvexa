package httpx

import (
	"errors"

	apperrors "github.com/Roy-Wanyoike/orvexa/pkg/errors"
)

// appErrorOf maps any error onto the canonical *apperrors.Error.
func appErrorOf(err error) *apperrors.Error {
	var e *apperrors.Error
	if errors.As(err, &e) {
		return e
	}
	return apperrors.From(err)
}

// StatusOf maps any error to its HTTP status (exported for handlers that
// need partial-outcome envelopes).
func StatusOf(err error) int {
	return appErrorOf(err).HTTPStatus()
}
