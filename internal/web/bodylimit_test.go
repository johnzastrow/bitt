package web

import (
	"io"
	"mime/multipart"
	"net/http"
	"sync/atomic"
	"testing"
)

// countingZeros streams n bytes of a multipart file part and counts how many
// the server actually pulled.
type countingReader struct {
	r    io.Reader
	read atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read.Add(int64(n))
	return n, err
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// postHuge streams a multipart upload of size bytes to path (field file, CSRF
// token included first) and returns how many bytes the server consumed.
func (h *harness) postHuge(t *testing.T, path, tokenPage, field string, size int64) int64 {
	t.Helper()
	token := h.csrfToken(tokenPage)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		_ = mw.WriteField("csrf_token", token)
		part, err := mw.CreateFormFile(field, "big.bin")
		if err == nil {
			_, err = io.Copy(part, io.LimitReader(zeros{}, size))
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	body := &countingReader{r: pr}
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.client.Do(req)
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	_ = pr.Close()
	return body.read.Load()
}

// The avatar route must stop reading an oversized upload near its limit. It
// used to read the CSRF token first, which parsed the entire body with no
// limit at all before the 2 MB one was installed.
func TestAvatarUploadBodyIsBounded(t *testing.T) {
	h := newHarness(t)
	h.completeSetup()
	const size = 64 << 20
	if got := h.postHuge(t, "/profile/avatar", "/profile", "avatar", size); got > 16<<20 {
		t.Errorf("server read %d MB of a %d MB upload; the limit is 2 MB", got>>20, size>>20)
	}
}
