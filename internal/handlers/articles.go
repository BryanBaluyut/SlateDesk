package handlers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/BryanBaluyut/slatedesk/internal/api"
	"github.com/BryanBaluyut/slatedesk/internal/email"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/store"
	"github.com/BryanBaluyut/slatedesk/internal/ticket"
)

// maxAttachmentBytes caps the uploaded file itself (spec: 25 MiB),
// enforced streaming — an oversize file is cut off mid-copy, never
// buffered.
const maxAttachmentBytes = 25 << 20

// multipartOverheadBytes is request-body slack for the multipart envelope
// (boundary lines + part headers), so a file of exactly maxAttachmentBytes
// — which the client advertises as acceptable — still fits under the
// http.MaxBytesReader cap.
const multipartOverheadBytes = 64 << 10

// fallbackContentType is stored when an upload names no (or a malformed)
// part content type.
const fallbackContentType = "application/octet-stream"

// toAPIArticle converts a store article row plus author display fields and
// pre-fetched attachment metadata.
func toAPIArticle(a store.Article, authorName, authorEmail string, attachments []api.Attachment) api.Article {
	out := api.Article{
		Id:          a.ID,
		TicketId:    a.TicketID,
		SenderType:  api.ArticleSenderType(a.SenderType),
		Channel:     api.ArticleChannel(a.Channel),
		IsInternal:  a.IsInternal,
		BodyText:    a.BodyText,
		Attachments: attachments,
		CreatedAt:   a.CreatedAt,
	}
	if a.AuthorID.Valid {
		id := uuid.UUID(a.AuthorID.Bytes)
		out.Author = &api.UserSummary{
			Id:    id,
			Name:  authorName,
			Email: openapi_types.Email(authorEmail),
		}
	}
	if a.BodyHtml.Valid {
		html := a.BodyHtml.String
		out.BodyHtml = &html
	}
	if a.DeliveryStatus.Valid {
		ds := api.DeliveryStatus(a.DeliveryStatus.String)
		out.DeliveryStatus = &ds
	}
	return out
}

// toAPIAttachment converts attachment metadata (never the storage key).
func toAPIAttachment(att store.ArticleAttachment) api.Attachment {
	return api.Attachment{
		Id:          att.ID,
		ArticleId:   att.ArticleID,
		Filename:    att.Filename,
		ContentType: att.ContentType,
		SizeBytes:   att.SizeBytes,
		CreatedAt:   att.CreatedAt,
	}
}

// CreateTicketArticle implements POST /tickets/{id}/articles (agent/admin):
// a public reply or, with is_internal, an internal note. The service applies
// the status matrix (public agent reply on an open ticket flips it to
// waiting_on_customer) and sanitizes body_html.
func (h *Handlers) CreateTicketArticle(w http.ResponseWriter, r *http.Request, id api.TicketID) {
	caller, ok := CurrentUser(r.Context())
	if !ok {
		problem.Write(w, r, http.StatusUnauthorized, "Unauthorized", "missing, invalid, or expired session")
		return
	}
	var req api.CreateTicketArticleJSONRequestBody
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.BodyText) == "" {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "body_text is required")
		return
	}
	bodyHTML := ""
	if req.BodyHtml != nil {
		bodyHTML = *req.BodyHtml
	}

	article, err := h.svc.AddArticle(r.Context(), id, ticket.ArticleInput{
		AuthorID:   &caller.ID,
		SenderType: store.ArticleSenderAgent,
		Channel:    store.ArticleChannelWeb,
		IsInternal: req.IsInternal,
		BodyText:   req.BodyText,
		BodyHTML:   bodyHTML,
	})
	if err != nil {
		writeTicketServiceError(w, r, "create article", err)
		return
	}
	writeJSON(w, r, http.StatusCreated, toAPIArticle(article, caller.Name, caller.Email, []api.Attachment{}))
}

// RetryArticleSend implements POST /articles/{id}/retry-send (agent/
// admin): re-enqueues the email delivery of an outbound article whose
// delivery_status is the terminal 'failed' — or stuck on 'sending' with
// no live send job left (worker crashed on the final attempt). The engine
// flips the badge back to 'queued' and inserts the email_send job in ONE
// transaction; the retried send reuses the Message-ID committed before
// the first attempt, so a duplicate delivery can never fork the thread.
func (h *Handlers) RetryArticleSend(w http.ResponseWriter, r *http.Request, id api.ArticleID) {
	if h.engine == nil {
		serverError(w, r, "retry article send", errors.New("email engine not configured"))
		return
	}
	article, err := h.engine.RetryFailedSend(r.Context(), id)
	if err != nil {
		switch {
		case isNoRows(err):
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such article")
		case errors.Is(err, email.ErrRetryNotFailed):
			problem.Write(w, r, http.StatusConflict, "Conflict",
				"the article's delivery is not in a retryable state")
		case errors.Is(err, email.ErrRetryNoMailbox):
			problem.Write(w, r, http.StatusConflict, "Conflict",
				"no mailbox is available to deliver this article")
		default:
			serverError(w, r, "retry article send", err)
		}
		return
	}

	authorName, authorEmail := "", ""
	if article.AuthorID.Valid {
		author, err := h.q.GetUserByID(r.Context(), uuid.UUID(article.AuthorID.Bytes))
		if err != nil && !isNoRows(err) {
			serverError(w, r, "load article author", err)
			return
		}
		if err == nil {
			authorName, authorEmail = author.Name, author.Email
		}
	}
	attRows, err := h.q.ListArticleAttachments(r.Context(), article.ID)
	if err != nil {
		serverError(w, r, "list article attachments", err)
		return
	}
	atts := make([]api.Attachment, 0, len(attRows))
	for _, att := range attRows {
		atts = append(atts, toAPIAttachment(att))
	}
	writeJSON(w, r, http.StatusAccepted, toAPIArticle(article, authorName, authorEmail, atts))
}

// sanitizeFilename reduces a client-supplied filename to a safe base name:
// path components (both separators) are stripped, control characters
// removed, and empty or dot-only results replaced with "attachment".
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, `\`, `/`)
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return "attachment"
	}
	const maxLen = 255
	if len(name) > maxLen {
		// Trim on a rune boundary.
		cut := maxLen
		for cut > 0 && (name[cut]&0xC0) == 0x80 {
			cut--
		}
		name = name[:cut]
	}
	return name
}

// sanitizeContentType returns a well-formed media type for storage/serving,
// falling back to application/octet-stream.
func sanitizeContentType(ct string) string {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return fallbackContentType
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return fallbackContentType
	}
	return mime.FormatMediaType(mediaType, params)
}

// isRequestTooLarge reports whether err came from the MaxBytesReader cap.
func isRequestTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// UploadArticleAttachment implements POST /articles/{id}/attachments
// (agent/admin): one multipart file part named "file", streamed to blob
// storage under a fresh opaque key, then recorded in article_attachments.
func (h *Handlers) UploadArticleAttachment(w http.ResponseWriter, r *http.Request, id api.ArticleID) {
	if _, err := h.q.GetArticle(r.Context(), id); err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such article")
			return
		}
		serverError(w, r, "load article", err)
		return
	}

	// Streaming size cap: the multipart reader below pulls straight from
	// this reader, so a grossly oversize upload dies near the 25 MiB mark
	// without ever being buffered. The exact per-file limit is enforced on
	// the part copy below; this cap only needs envelope slack on top.
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentBytes+multipartOverheadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		problem.Write(w, r, http.StatusBadRequest, "Bad Request", "multipart/form-data body required")
		return
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", `missing "file" part`)
			return
		}
		if err != nil {
			if isRequestTooLarge(err) {
				problem.Write(w, r, http.StatusRequestEntityTooLarge, "Content Too Large",
					fmt.Sprintf("attachment upload exceeds %d bytes", maxAttachmentBytes))
				return
			}
			problem.Write(w, r, http.StatusBadRequest, "Bad Request", "malformed multipart body")
			return
		}
		if part.FormName() != "file" {
			continue
		}

		filename := sanitizeFilename(part.FileName())
		contentType := sanitizeContentType(part.Header.Get("Content-Type"))

		key := storage.NewKey()
		// Read at most one byte over the per-file limit: an oversize file
		// becomes a clean 413 after the check below (the request-body cap
		// above has envelope slack, so it alone cannot enforce the file
		// limit exactly).
		size, err := h.blobs.Save(r.Context(), key, io.LimitReader(part, maxAttachmentBytes+1))
		if err != nil {
			if isRequestTooLarge(err) {
				problem.Write(w, r, http.StatusRequestEntityTooLarge, "Content Too Large",
					fmt.Sprintf("attachment upload exceeds %d bytes", maxAttachmentBytes))
				return
			}
			serverError(w, r, "store attachment blob", err)
			return
		}
		if size > maxAttachmentBytes {
			if delErr := h.blobs.Delete(context.WithoutCancel(r.Context()), key); delErr != nil {
				slog.Warn("delete oversize attachment blob", "key", key, "error", delErr)
			}
			problem.Write(w, r, http.StatusRequestEntityTooLarge, "Content Too Large",
				fmt.Sprintf("attachment upload exceeds %d bytes", maxAttachmentBytes))
			return
		}

		att, err := h.q.CreateArticleAttachment(r.Context(), store.CreateArticleAttachmentParams{
			ArticleID:   id,
			Filename:    filename,
			ContentType: contentType,
			SizeBytes:   size,
			StorageKey:  key,
		})
		if err != nil {
			// Best-effort blob cleanup; an orphaned blob is only wasted disk.
			if delErr := h.blobs.Delete(context.WithoutCancel(r.Context()), key); delErr != nil {
				slog.Warn("orphaned attachment blob after failed insert", "key", key, "error", delErr)
			}
			serverError(w, r, "record attachment", err)
			return
		}
		writeJSON(w, r, http.StatusCreated, toAPIAttachment(att))
		return
	}
}

// DownloadAttachment implements GET /attachments/{id} (agent/admin):
// streams the blob with the stored content type and a forced-download
// Content-Disposition carrying the sanitized filename.
func (h *Handlers) DownloadAttachment(w http.ResponseWriter, r *http.Request, id api.AttachmentID) {
	att, err := h.q.GetArticleAttachment(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such attachment")
			return
		}
		serverError(w, r, "load attachment", err)
		return
	}
	if err := storage.ValidateKey(att.StorageKey); err != nil {
		serverError(w, r, "validate storage key", err)
		return
	}
	blob, err := h.blobs.Open(r.Context(), att.StorageKey)
	if err != nil {
		// A row without its blob is an integrity fault, not a client 404.
		serverError(w, r, "open attachment blob", err)
		return
	}
	defer func() { _ = blob.Close() }()

	w.Header().Set("Content-Type", sanitizeContentType(att.ContentType))
	w.Header().Set("Content-Length", strconv.FormatInt(att.SizeBytes, 10))
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": sanitizeFilename(att.Filename)}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, blob); err != nil {
		// Headers are gone; nothing to send the client. Log and move on.
		slog.Warn("stream attachment", "id", att.ID, "error", err)
	}
}
