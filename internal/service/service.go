// Package service holds the business rules; transports (HTTP today) stay thin.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"

	"ttlbox/internal/storage"
)

var (
	ErrBadTTL    = errors.New("ttl out of range")
	ErrForbidden = errors.New("forbidden")
)

type Service struct {
	store          storage.Storage
	defTTL, maxTTL time.Duration
}

func New(s storage.Storage, defTTL, maxTTL time.Duration) *Service {
	return &Service{store: s, defTTL: defTTL, maxTTL: maxTTL}
}

type Upload struct {
	Name string
	Text bool
	TTL  time.Duration // 0 = default
	Body io.Reader
}

type Created struct {
	ID          string
	DeleteToken string
	Expires     time.Time
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (s *Service) Create(ctx context.Context, in Upload) (Created, error) {
	ttl := in.TTL
	if ttl == 0 {
		ttl = s.defTTL
	}
	if ttl < time.Second || ttl > s.maxTTL {
		return Created{}, ErrBadTTL
	}
	tok := make([]byte, 16)
	if _, err := rand.Read(tok); err != nil {
		return Created{}, err
	}
	token := hex.EncodeToString(tok)
	m := storage.Meta{
		Name:       cleanName(in.Name),
		Text:       in.Text,
		Expires:    time.Now().Add(ttl),
		DeleteHash: hash(token),
	}
	if in.Text {
		m.Name = "paste.txt"
	}
	id := storage.NewID()
	if _, err := s.store.Put(ctx, id, m, in.Body); err != nil {
		return Created{}, err
	}
	return Created{ID: id, DeleteToken: token, Expires: m.Expires}, nil
}

func (s *Service) Open(ctx context.Context, id string) (storage.Meta, io.ReadSeekCloser, error) {
	return s.store.Open(ctx, id)
}

func (s *Service) Remove(ctx context.Context, id, token string) error {
	m, err := s.store.Stat(ctx, id)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(hash(token)), []byte(m.DeleteHash)) != 1 {
		return ErrForbidden
	}
	return s.store.Delete(ctx, id)
}

func (s *Service) Sweep(ctx context.Context) (int, error) { return s.store.Sweep(ctx) }

func cleanName(n string) string {
	n = strings.ToValidUTF8(n, "")
	n = n[strings.LastIndexAny(n, `/\`)+1:]
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, n)
	if r := []rune(n); len(r) > 100 {
		n = string(r[:100])
	}
	if n == "" || n == "." || n == ".." {
		return "file"
	}
	return n
}
