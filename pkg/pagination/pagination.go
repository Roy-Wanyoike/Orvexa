// Package pagination implements bounded list windows across the API.
//
// Orvexa uses cursor pagination (opaque, base64 of "offset|limit") for stable
// forward-only iteration, with hard upper bounds on page size so no client can
// request an unbounded query.
package pagination

import (
        "encoding/base64"
        "fmt"
        "strconv"
        "strings"
)

const (
        DefaultLimit = 25
        MaxLimit     = 100
)

// Page is a decoded page request.
type Page struct {
        Offset int
        Limit  int
}

// Parse decodes a cursor + limit pair, clamping limit into [1, MaxLimit].
// An empty cursor starts from offset 0 with DefaultLimit (or the requested limit).
func Parse(cursor string, limit int) (Page, error) {
        p := Page{Limit: clamp(limit)}
        if cursor == "" {
                return p, nil
        }
        raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(cursor))
        if err != nil {
                return Page{}, fmt.Errorf("malformed cursor")
        }
        parts := strings.SplitN(string(raw), "|", 2)
        if len(parts) != 2 {
                return Page{}, fmt.Errorf("malformed cursor")
        }
        off, err := strconv.Atoi(parts[0])
        if err != nil || off < 0 {
                return Page{}, fmt.Errorf("malformed cursor")
        }
        lim, err := strconv.Atoi(parts[1])
        if err != nil || lim <= 0 {
                return Page{}, fmt.Errorf("malformed cursor")
        }
        p.Offset, p.Limit = off, clamp(lim)
        return p, nil
}

func clamp(n int) int {
        if n <= 0 {
                return DefaultLimit
        }
        if n > MaxLimit {
                return MaxLimit
        }
        return n
}

// NextCursor encodes the cursor for the following page, or "" when the page
// was not full (i.e. iteration is complete).
func (p Page) NextCursor(returned int) string {
        if returned < p.Limit {
                return ""
        }
        return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d|%d", p.Offset+p.Limit, p.Limit)))
}
