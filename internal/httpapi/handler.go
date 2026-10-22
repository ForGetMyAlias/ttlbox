// Package httpapi is the HTTP transport: routing, auth, limits and security headers.
package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"ttlbox/internal/dur"
	"ttlbox/internal/ratelimit"
	"ttlbox/internal/service"
	"ttlbox/internal/storage"
)

const (
	maxText  = 1 << 20 // pasted text cap
	maxField = 256     // ttl/token form fields
)

type Options struct {
	BaseURL                    string // public URL for returned links; derived from the request if empty
	Token                      string // required for uploads when non-empty
	MaxBody                    int64
	TrustProxy                 bool // believe X-Forwarded-* from a reverse proxy in front
	UploadLimit, DownloadLimit *ratelimit.Limiter
	Log                        *slog.Logger
}

type handler struct {
	svc *service.Service
	o   Options
}

func New(svc *service.Service, o Options) http.Handler {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	h := &handler{svc: svc, o: o}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("POST /upload", h.limit(o.UploadLimit, h.upload))
	mux.HandleFunc("PUT /upload/{name}", h.limit(o.UploadLimit, h.put))
	mux.HandleFunc("GET /f/{id}", h.limit(o.DownloadLimit, h.download))
	mux.HandleFunc("DELETE /f/{id}", h.limit(o.DownloadLimit, h.remove))
	return h.recoverer(h.logger(h.secure(mux)))
}

func bearer(r *http.Request) string {
	t, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return t
}

// authed checks the upload token; hashing first makes the compare length-independent.
func (h *handler) authed(w http.ResponseWriter, token string) bool {
	if h.o.Token == "" {
		return true
	}
	a, b := sha256.Sum256([]byte(token)), sha256.Sum256([]byte(h.o.Token))
	if subtle.ConstantTimeCompare(a[:], b[:]) == 1 {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="ttlbox"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (h *handler) fail(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &mbe):
		http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
	case errors.Is(err, service.ErrBadTTL):
		http.Error(w, "ttl out of range", http.StatusBadRequest)
	case errors.Is(err, storage.ErrFull):
		http.Error(w, "server storage full", http.StatusInsufficientStorage)
	case errors.Is(err, storage.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, service.ErrForbidden):
		http.Error(w, "forbidden", http.StatusForbidden)
	default:
		h.o.Log.Error("request failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := dur.Parse(s)
	if err != nil {
		return 0, service.ErrBadTTL
	}
	return d, nil
}

// upload streams multipart/form-data. Fields "ttl" and "token" must precede
// the "file"/"text" part so the upload is authorised before any bytes are stored.
func (h *handler) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.o.MaxBody)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected multipart/form-data", http.StatusBadRequest)
		return
	}
	token, ttl := bearer(r), ""
	for {
		p, err := mr.NextPart()
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				h.fail(w, err)
			} else {
				http.Error(w, "need a 'file' or 'text' part", http.StatusBadRequest)
			}
			return
		}
		var in service.Upload
		switch p.FormName() {
		case "ttl", "token":
			b, err := io.ReadAll(io.LimitReader(p, maxField+1))
			if err != nil || len(b) > maxField {
				http.Error(w, "bad field", http.StatusBadRequest)
				return
			}
			if p.FormName() == "ttl" {
				ttl = string(b)
			} else {
				token = string(b)
			}
			continue
		case "text":
			b, err := io.ReadAll(io.LimitReader(p, maxText+1))
			if err != nil {
				h.fail(w, err)
				return
			}
			if len(b) == 0 {
				continue
			}
			if len(b) > maxText {
				http.Error(w, "text too large", http.StatusRequestEntityTooLarge)
				return
			}
			in = service.Upload{Text: true, Body: bytes.NewReader(b)}
		case "file":
			if p.FileName() == "" {
				continue
			}
			in = service.Upload{Name: p.FileName(), Body: p}
		default:
			continue
		}
		if !h.authed(w, token) {
			return
		}
		h.create(w, r, ttl, in)
		return
	}
}

// put supports `curl -T file URL/upload/name?ttl=1h`.
func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.o.MaxBody)
	if !h.authed(w, bearer(r)) {
		return
	}
	h.create(w, r, r.URL.Query().Get("ttl"), service.Upload{Name: r.PathValue("name"), Body: r.Body})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request, ttl string, in service.Upload) {
	d, err := parseTTL(ttl)
	if err == nil {
		in.TTL = d
		var c service.Created
		if c, err = h.svc.Create(r.Context(), in); err == nil {
			h.created(w, r, c)
			return
		}
	}
	h.fail(w, err)
}

func (h *handler) created(w http.ResponseWriter, r *http.Request, c service.Created) {
	url := h.baseURL(r) + "/f/" + c.ID
	exp := c.Expires.UTC().Format(time.RFC3339)
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": url, "delete_token": c.DeleteToken, "expires": exp})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%s\ndelete-token: %s\nexpires: %s\n", url, c.DeleteToken, exp)
}

func (h *handler) download(w http.ResponseWriter, r *http.Request) {
	m, f, err := h.svc.Open(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, err)
		return
	}
	defer f.Close()
	hd := w.Header()
	hd.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	hd.Set("Cache-Control", "private, no-store")
	hd.Set("X-Expires", m.Expires.UTC().Format(time.RFC3339))
	if m.Text {
		hd.Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		hd.Set("Content-Type", "application/octet-stream")
		hd.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": m.Name}))
	}
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Remove(r.Context(), r.PathValue("id"), r.Header.Get("X-Delete-Token")); err != nil {
		h.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
