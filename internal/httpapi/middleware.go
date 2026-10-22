package httpapi

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"ttlbox/internal/ratelimit"
)

func (h *handler) https(r *http.Request) bool {
	return r.TLS != nil || (h.o.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
}

func (h *handler) baseURL(r *http.Request) string {
	if h.o.BaseURL != "" {
		return h.o.BaseURL
	}
	scheme := "http"
	if h.https(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// clientIP is the rate-limit key. Behind a trusted proxy it is the last (proxy-appended)
// X-Forwarded-For entry, which clients cannot forge. IPv6 is bucketed per /64.
func (h *handler) clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if h.o.TrustProxy {
		if xff := strings.Join(r.Header.Values("X-Forwarded-For"), ","); xff != "" {
			ip = xff[strings.LastIndex(xff, ",")+1:]
		}
	}
	ip = strings.TrimSpace(ip)
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	if a = a.Unmap(); a.Is6() {
		p, _ := a.Prefix(64)
		return p.String()
	}
	return a.String()
}

func (h *handler) limit(l *ratelimit.Limiter, next http.HandlerFunc) http.HandlerFunc {
	if l == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(h.clientIP(r)) {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

func (h *handler) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		hd.Set("X-Robots-Tag", "noindex, nofollow")
		hd.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if h.https(r) {
			hd.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(c int)           { s.status = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// logger deliberately omits the path: download URLs are capabilities.
func (h *handler) logger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		h.o.Log.Info("request", "method", r.Method, "status", sw.status, "ms", time.Since(start).Milliseconds(), "ip", h.clientIP(r))
	})
}

func (h *handler) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				h.o.Log.Error("panic", "value", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
