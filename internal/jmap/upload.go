package jmap

import (
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/mikepage/robinmail/internal/store"
)

// serveUpload is the upload endpoint (RFC 8620 6.1): the body of the POST
// becomes a blob of the account, named by the SHA-256 of its content. The
// blob can then be used in Email/set (as a body part), Email/import and
// Email/parse, and downloaded. One that nothing refers to is removed after
// a day.
func (h *Handler) serveUpload(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	if r.PathValue("accountId") != u.AccountID {
		problem(w, http.StatusNotFound, "about:blank", "no such account", nil)
		return
	}
	h.mu.Lock()
	if h.uploads[u.AccountID] >= h.limits.MaxConcurrentUpload {
		h.mu.Unlock()
		problem(w, http.StatusBadRequest, errLimit, "too many uploads at once for this account", map[string]any{"limit": "maxConcurrentUpload"})
		return
	}
	h.uploads[u.AccountID]++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.uploads[u.AccountID]--; h.uploads[u.AccountID] <= 0 {
			delete(h.uploads, u.AccountID)
		}
		h.mu.Unlock()
	}()

	if r.ContentLength > h.limits.MaxSizeUpload {
		problem(w, http.StatusRequestEntityTooLarge, errLimit, "the upload is larger than maxSizeUpload", map[string]any{"limit": "maxSizeUpload"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, h.limits.MaxSizeUpload+1))
	if err != nil {
		problem(w, http.StatusBadRequest, "about:blank", "the upload could not be read", nil)
		return
	}
	if int64(len(data)) > h.limits.MaxSizeUpload {
		problem(w, http.StatusRequestEntityTooLarge, errLimit, "the upload is larger than maxSizeUpload", map[string]any{"limit": "maxSizeUpload"})
		return
	}
	// The type is the client's word and is only handed back.
	typ := "application/octet-stream"
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && len(mt) <= 100 {
		typ = mt
	}
	blobID, err := h.Store.Upload(r.Context(), u.AccountID, data, typ)
	if errors.Is(err, store.ErrOverQuota) {
		// RFC 8620 6.1 names no status for this; 507 is HTTP's for "the
		// server is unable to store the representation" (RFC 4918 11.5).
		problem(w, http.StatusInsufficientStorage, "about:blank", "the account is full", nil)
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "about:blank", "the server could not store the upload", nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"accountId": u.AccountID, "blobId": blobID, "type": typ, "size": len(data)})
}
