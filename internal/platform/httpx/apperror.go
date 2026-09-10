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
