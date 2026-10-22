// Package config reads and validates environment configuration; bad values fail startup.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"ttlbox/internal/dur"
)

type Config struct {
	Addr, DataDir, Token, BaseURL string
	MaxSize, MaxTotal             int64 // bytes; MaxTotal 0 = unlimited
	DefaultTTL, MaxTTL            time.Duration
	TrustProxy                    bool
	UploadRate, DownloadRate      int // requests/minute per client, 0 = off
}

type loader struct{ errs []error }

func (l *loader) fail(k string, err error) { l.errs = append(l.errs, fmt.Errorf("%s: %w", k, err)) }

func (l *loader) str(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (l *loader) int(k string, d int) int {
	v := l.str(k, strconv.Itoa(d))
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		l.fail(k, errors.New("want a non-negative integer"))
	}
	return n
}

func (l *loader) dur(k, d string) time.Duration {
	v, err := dur.Parse(l.str(k, d))
	if err != nil {
		l.fail(k, err)
	}
	return v
}

func (l *loader) bool(k string) bool {
	v, err := strconv.ParseBool(l.str(k, "false"))
	if err != nil {
		l.fail(k, err)
	}
	return v
}

func Load() (Config, error) {
	var l loader
	c := Config{
		Addr:         l.str("ADDR", ":8080"),
		DataDir:      l.str("DATA_DIR", "./data"),
		Token:        os.Getenv("UPLOAD_TOKEN"),
		BaseURL:      strings.TrimRight(os.Getenv("BASE_URL"), "/"),
		MaxSize:      int64(l.int("MAX_SIZE_MB", 50)) << 20,
		MaxTotal:     int64(l.int("MAX_TOTAL_MB", 0)) << 20,
		DefaultTTL:   l.dur("DEFAULT_TTL", "24h"),
		MaxTTL:       l.dur("MAX_TTL", "7d"),
		TrustProxy:   l.bool("TRUST_PROXY"),
		UploadRate:   l.int("UPLOAD_RATE", 20),
		DownloadRate: l.int("DOWNLOAD_RATE", 300),
	}
	if c.MaxSize == 0 {
		l.fail("MAX_SIZE_MB", errors.New("must be > 0"))
	}
	if c.DefaultTTL > c.MaxTTL {
		l.fail("DEFAULT_TTL", errors.New("exceeds MAX_TTL"))
	}
	if c.Token != "" && len(c.Token) < 16 {
		l.fail("UPLOAD_TOKEN", errors.New("must be at least 16 characters"))
	}
	if c.BaseURL != "" {
		if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			l.fail("BASE_URL", errors.New("want http(s)://host"))
		}
	}
	return c, errors.Join(l.errs...)
}
