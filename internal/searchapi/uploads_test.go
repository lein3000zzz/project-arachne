package searchapi

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"web-crawler/internal/uploads"
)

func multipartBody(t *testing.T, parts map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()

	var body bytes.Buffer

	w := multipart.NewWriter(&body)

	for name, data := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="files"; filename="`+name+`"`)

		if strings.HasSuffix(name, ".pdf") {
			h.Set("Content-Type", "application/pdf")
		}

		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}

		_, _ = part.Write(data)
	}

	_ = w.Close()

	return &body, w.FormDataContentType()
}

func TestUploadsReachTheLabAsFiles(t *testing.T) {
	service := &fakeLabs{}
	handler := newTestHandler(Services{Labs: service}, testConfig())

	body, kind := multipartBody(t, map[string][]byte{"C:\\papers\\resnet.pdf": []byte("%PDF-1.5"), "notes.md": []byte("# notes")})
	req := signedIn(httptest.NewRequest(http.MethodPost, "/api/labs/4/uploads", body))
	req.Header.Set("Content-Type", kind)

	if rec := do(handler, req); rec.Code != http.StatusOK || service.owner != testUser.ID || service.labIDs[0] != 4 || len(service.files) != 2 {
		t.Fatalf("upload: %d %s, files %+v", rec.Code, rec.Body, service.files)
	}

	byName := map[string]string{}
	for _, f := range service.files {
		byName[f.Name] = f.ContentType + " " + string(f.Data)
	}

	// What .md maps to depends on the system's MIME table; sniffing gives text.
	if byName["resnet.pdf"] != "application/pdf %PDF-1.5" || !strings.HasPrefix(byName["notes.md"], "text/") || !strings.HasSuffix(byName["notes.md"], " # notes") {
		t.Errorf("files: %v; want the base name, the declared or guessed type, the bytes", byName)
	}

	big, kind := multipartBody(t, map[string][]byte{"huge.pdf": bytes.Repeat([]byte("x"), uploads.MaxBytes+1)})
	req = signedIn(httptest.NewRequest(http.MethodPost, "/api/labs/4/uploads", big))
	req.Header.Set("Content-Type", kind)

	if rec := do(handler, req); rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "huge.pdf") {
		t.Errorf("over the limit: %d %s", rec.Code, rec.Body)
	}

	if rec := serveLabs(service, http.MethodPost, "/api/labs/4/uploads", `{"files": []}`); rec.Code != http.StatusBadRequest {
		t.Errorf("JSON instead of files: %d", rec.Code)
	}
}

type fakeUploads struct{ files map[string]uploads.File }

func (f fakeUploads) Owned(_ context.Context, owner int64, id string) (uploads.File, error) {
	file, ok := f.files[id]
	if !ok || owner != testUser.ID {
		return uploads.File{}, uploads.ErrNotFound
	}

	return file, nil
}

func TestAnUploadIsServedToItsOwnerOnly(t *testing.T) {
	handler := newTestHandler(Services{Uploads: fakeUploads{files: map[string]uploads.File{
		"pdf":  {Name: "résumé.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.5")},
		"html": {Name: "page.html", ContentType: "text/html", Data: []byte("<script>alert(1)</script>")},
	}}}, testConfig())

	rec := do(handler, withSession(httptest.NewRequest(http.MethodGet, "/uploads/pdf", nil)))
	if rec.Code != http.StatusOK || rec.Body.String() != "%PDF-1.5" || rec.Header().Get("Content-Type") != "application/pdf" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "inline") || rec.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Errorf("pdf: %d %v", rec.Code, rec.Header())
	}

	rec = do(handler, withSession(httptest.NewRequest(http.MethodGet, "/uploads/html", nil)))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("an uploaded page must be downloaded, never shown on this origin: %d %v", rec.Code, rec.Header())
	}

	if rec := do(handler, withSession(httptest.NewRequest(http.MethodGet, "/uploads/someone-elses", nil))); rec.Code != http.StatusNotFound {
		t.Errorf("not theirs: %d", rec.Code)
	}

	if rec := do(handler, httptest.NewRequest(http.MethodGet, "/uploads/pdf", nil)); rec.Code != http.StatusSeeOther && rec.Code != http.StatusFound {
		t.Errorf("signed out: %d, want a redirect to the login page", rec.Code)
	}
}
