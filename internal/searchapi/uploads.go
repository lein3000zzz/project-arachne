package searchapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"web-crawler/internal/auth"
	"web-crawler/internal/labs"
	"web-crawler/internal/uploads"

	"go.uber.org/zap"
)

type UploadStore interface {
	Owned(ctx context.Context, ownerID int64, id string) (uploads.File, error)
}

const (
	maxUploadFiles    = 10
	maxFilenameRunes  = 200
	uploadTimeout     = time.Minute
	multipartOverhead = 64 << 10
)

// addUploads reads the files one at a time, each whole: the store keeps a
// file in one row.
func (h *labsHandler) addUploads(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadFiles*(uploads.MaxBytes+multipartOverhead))

	reader, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: `send the files as multipart/form-data, each in a part named "files"`})
		return
	}

	var files []labs.File

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "the upload was cut short or is too large"})
			return
		}

		if part.FormName() != "files" {
			continue
		}

		if len(files) == maxUploadFiles {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("at most %d files at a time", maxUploadFiles)})
			return
		}

		name := filename(part.FileName())

		data, err := io.ReadAll(io.LimitReader(part, uploads.MaxBytes+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "the upload was cut short or is too large"})
			return
		}

		if len(data) > uploads.MaxBytes {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Error: fmt.Sprintf("%s is over %d MiB", name, uploads.MaxBytes>>20)})
			return
		}

		files = append(files, labs.File{Name: name, ContentType: contentType(part.Header.Get("Content-Type"), name, data), Data: data})
	}

	h.withLabFor(w, r, uploadTimeout, func(ctx context.Context, owner, lab int64) (int, any, error) {
		detail, err := h.service.AddUploads(ctx, owner, lab, files)
		return http.StatusOK, detail, err
	})
}

// filename keeps the name a file was uploaded under, without any path a
// browser sent with it.
func filename(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, `\`, "/")))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}

		return r
	}, name)

	if utf8.RuneCountInString(name) > maxFilenameRunes {
		name = string([]rune(name)[:maxFilenameRunes])
	}

	if name == "" || name == "." || name == "/" {
		return "upload"
	}

	return name
}

// contentType is what the browser said, else what the name says, else what
// the bytes look like. Tika sniffs on its own; this is for serving it back.
func contentType(declared, name string, data []byte) string {
	if t, _, err := mime.ParseMediaType(declared); err == nil && t != "application/octet-stream" {
		return t
	}

	if t, _, err := mime.ParseMediaType(mime.TypeByExtension(strings.ToLower(filepath.Ext(name)))); err == nil {
		return t
	}

	t, _, _ := mime.ParseMediaType(http.DetectContentType(data))

	return t
}

type uploadsHandler struct {
	logger *zap.SugaredLogger
	files  UploadStore
}

// Shown in the browser; anything else is downloaded. An uploaded HTML page
// shown on this origin would run its scripts as the signed-in user.
var inlineTypes = map[string]bool{
	"application/pdf": true, "text/plain": true, "image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
}

// serve gives an uploaded file back to its owner: citations link here.
func (h *uploadsHandler) serve(w http.ResponseWriter, r *http.Request) {
	if h.files == nil {
		http.NotFound(w, r)
		return
	}

	user, _ := auth.UserFrom(r.Context())

	f, err := h.files.Owned(r.Context(), user.ID, r.PathValue("id"))

	switch {
	case errors.Is(err, uploads.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		h.logger.Errorw("Reading an upload failed", "error", err)
		http.Error(w, "the file could not be read", http.StatusInternalServerError)

		return
	}

	disposition, kind := "attachment", "application/octet-stream"
	if inlineTypes[f.ContentType] {
		disposition, kind = "inline", f.ContentType
	}

	if header := mime.FormatMediaType(disposition, map[string]string{"filename": f.Name}); header != "" {
		w.Header().Set("Content-Disposition", header)
	} else {
		w.Header().Set("Content-Disposition", disposition)
	}

	w.Header().Set("Content-Type", kind)
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, no-store")

	http.ServeContent(w, r, f.Name, time.Time{}, bytes.NewReader(f.Data))
}
