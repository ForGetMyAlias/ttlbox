// Package storage defines where uploads live. Implement Storage to move blobs
// to S3 or metadata to a database without touching the rest of the service.
package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"regexp"
	"time"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrFull      = errors.New("storage full")
	errInvalidID = errors.New("invalid id")
)

type Meta struct {
	Name       string    `json:"name"`
	Text       bool      `json:"text"`
	Size       int64     `json:"size"`
	Expires    time.Time `json:"expires"`
	DeleteHash string    `json:"delete_hash"` // sha256 of the delete token
}

// Storage implementations must be safe for concurrent use. Expired items must
// behave as ErrNotFound; Sweep reclaims their space and is safe to run from
// several instances at once.
type Storage interface {
	Put(ctx context.Context, id string, m Meta, r io.Reader) (Meta, error)
	Stat(ctx context.Context, id string) (Meta, error)
	Open(ctx context.Context, id string) (Meta, io.ReadSeekCloser, error)
	Delete(ctx context.Context, id string) error
	Sweep(ctx context.Context) (removed int, err error)
}

const (
	idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789" // lowercase: safe on case-insensitive filesystems
	idLen      = 12
)

var idRe = regexp.MustCompile(`^[a-z0-9]{12}$`)

// ValidID is the only shape of id that may ever touch the filesystem.
func ValidID(id string) bool { return idRe.MatchString(id) }

// NewID returns 12 random base36 chars (~62 bits): short, and still infeasible to
// guess under the per-IP rate limits and expiry. Bytes >= 252 are skipped so
// every character is equally likely.
func NewID() string {
	id := make([]byte, 0, idLen)
	buf := make([]byte, idLen)
	for len(id) < idLen {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			if b < 252 && len(id) < idLen {
				id = append(id, idAlphabet[b%36])
			}
		}
	}
	return string(id)
}
