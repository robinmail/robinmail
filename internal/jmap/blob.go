package jmap

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/robinmail/robinmail/internal/message"
	"github.com/robinmail/robinmail/internal/store"
)

// maxInlineImage is the largest image of a message that the HTML view
// embeds for a "cid:" reference.
const maxInlineImage = 2 << 20

// inlineImageTypes are the image types the view embeds. No SVG.
var inlineImageTypes = map[string]bool{"image/png": true, "image/gif": true, "image/jpeg": true, "image/webp": true, "image/bmp": true}

// splitBlobID takes a blob id apart: the blob of a message or an upload,
// and after each dash the id of a part. "<blob>-3" is part 3 of the
// message; "<blob>-3-2" is part 2 of the message that is attached as part
// 3. part is everything after the first dash.
func splitBlobID(id string) (blob, part string, ok bool) {
	blob, part, _ = strings.Cut(id, "-")
	if len(blob) != 64 || len(part) > 3*7 {
		return "", "", false
	}
	for i := 0; i < len(blob); i++ {
		if c := blob[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", "", false
		}
	}
	return blob, part, true
}

// readBlob reads a whole blob. Its size is bounded by what SMTP and the
// upload endpoint accept.
func (h *Handler) readBlob(ctx context.Context, account, blob string) ([]byte, error) {
	rc, _, err := h.Store.OpenBlob(ctx, account, blob)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// blobData resolves any blob id of the account to its content: a message
// or upload, or a body part inside one. store.ErrNoBlob if there is none.
func (h *Handler) blobData(ctx context.Context, account, blobID string) ([]byte, error) {
	blob, part, ok := splitBlobID(blobID)
	if !ok {
		return nil, store.ErrNoBlob
	}
	data, err := h.readBlob(ctx, account, blob)
	if err != nil {
		return nil, err
	}
	for part != "" {
		var id string
		id, part, _ = strings.Cut(part, "-")
		p := message.Parse(data).Part(id)
		if p == nil {
			return nil, store.ErrNoBlob
		}
		data = p.Content()
	}
	return data, nil
}

// safeDownloadType accepts a "type/subtype" to send a download as, and
// falls back to application/octet-stream.
func safeDownloadType(t string) string {
	mt, _, err := mime.ParseMediaType(t)
	if err != nil || !strings.Contains(mt, "/") || len(t) > 100 {
		return "application/octet-stream"
	}
	return mt
}

// serveDownload is the download endpoint (RFC 8620 6.2): a whole message
// by the blobId of its Email, or one body part by the blobId of the part.
//
// What comes out is mail from strangers served from our own origin, so it
// is always an attachment, never sniffed, and sandboxed should a browser
// show it anyway. An <img> in the webmail can still load an image part.
func (h *Handler) serveDownload(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	if r.PathValue("accountId") != u.AccountID {
		problem(w, http.StatusNotFound, "about:blank", "no such account", nil)
		return
	}
	blob, part, ok := splitBlobID(r.PathValue("blobId"))
	if !ok {
		problem(w, http.StatusNotFound, "about:blank", "no such blob", nil)
		return
	}
	name := r.PathValue("name")
	hdr := w.Header()
	hdr.Set("Content-Type", safeDownloadType(r.URL.Query().Get("type")))
	hdr.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// RFC 8620 6.2: a blob never changes, so the browser may keep it.
	hdr.Set("Cache-Control", "private, immutable, max-age=31536000")

	if part == "" {
		rc, size, err := h.Store.OpenBlob(r.Context(), u.AccountID, blob)
		if err != nil {
			h.blobError(w, err)
			return
		}
		defer rc.Close()
		hdr.Set("Content-Length", strconv.FormatInt(size, 10))
		io.Copy(w, rc)
		return
	}
	data, err := h.blobData(r.Context(), u.AccountID, r.PathValue("blobId"))
	if err != nil {
		h.blobError(w, err)
		return
	}
	hdr.Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

func (h *Handler) blobError(w http.ResponseWriter, err error) {
	hdr := w.Header()
	for _, k := range []string{"Content-Disposition", "Content-Length"} {
		hdr.Del(k)
	}
	hdr.Set("Cache-Control", "no-store")
	if errors.Is(err, store.ErrNoBlob) || errors.Is(err, store.ErrNoEmail) {
		problem(w, http.StatusNotFound, "about:blank", "no such blob", nil)
		return
	}
	problem(w, http.StatusInternalServerError, "about:blank", "the server could not read the blob", nil)
}

// RemoteImagesHeader tells the webmail what the view did about remote
// images: "blocked" (there are some and they were left out), "shown", or
// "none".
const RemoteImagesHeader = "X-Remote-Images"

// serveView renders the HTML body of an email for a sandboxed iframe:
//
//	GET /jmap/view/{accountId}/{emailId}[?images=1]
//
// It is not part of JMAP. The HTML is sanitised (message.SanitizeHTML) and
// served with a Content-Security-Policy that allows no script and no
// remote loads; images=1 lets images load over the network. Images of the
// message itself ("cid:") are embedded. A message without HTML is shown as
// its plain text.
func (h *Handler) serveView(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	id, ok := store.ParseEmailID(r.PathValue("emailId"))
	if r.PathValue("accountId") != u.AccountID || !ok {
		problem(w, http.StatusNotFound, "about:blank", "no such email", nil)
		return
	}
	raw, _, err := h.Store.Raw(r.Context(), u.AccountID, id)
	if errors.Is(err, store.ErrNoEmail) {
		problem(w, http.StatusNotFound, "about:blank", "no such email", nil)
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "about:blank", "the server could not read the email", nil)
		return
	}
	remote := r.URL.Query().Get("images") == "1"
	msg := message.Parse(raw)
	_, htmlParts, _ := msg.Bodies()
	opt := message.ViewOptions{RemoteImages: remote, CID: func(cid string) string {
		for _, p := range msg.Leaves {
			if p.CID != nil && *p.CID == cid && inlineImageTypes[p.Type] && p.Size <= maxInlineImage {
				return "data:" + p.Type + ";base64," + base64.StdEncoding.EncodeToString(p.Content())
			}
		}
		return ""
	}}
	var body strings.Builder
	blocked := false
	for _, p := range htmlParts {
		text, _ := p.Text()
		switch p.Type {
		case "text/html":
			v := message.SanitizeHTML(text, opt)
			blocked = blocked || v.BlockedRemote
			body.WriteString(v.HTML)
		case "text/plain":
			body.WriteString(message.TextView(text))
		}
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Security-Policy", message.ViewCSP(remote))
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	// The webmail frames it; nobody else may.
	hdr.Set("X-Frame-Options", "SAMEORIGIN")
	switch {
	case blocked:
		hdr.Set(RemoteImagesHeader, "blocked")
	case remote:
		hdr.Set(RemoteImagesHeader, "shown")
	default:
		hdr.Set(RemoteImagesHeader, "none")
	}
	io.WriteString(w, message.ViewDocument(body.String()))
}
