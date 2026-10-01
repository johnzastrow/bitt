package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/johnzastrow/bitt/internal/bankcsv"
	"github.com/johnzastrow/bitt/internal/store"
)

// RECON-02: importing a CSV, mapping a new layout once, and duplicates.

func referenceCSV(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../bankcsv/testdata/reference.csv")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// reconcileReady completes setup and gives Jane (id 1) the permission.
func reconcileReady(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.completeSetup()
	h.setReconcile(1, true)
	return h
}

// upload posts a statement and follows redirects.
func (h *harness) upload(t *testing.T, name string, data []byte) (*http.Response, string) {
	t.Helper()
	return h.uploadToken(t, name, data, h.csrfToken("/admin/reconcile"))
}

func (h *harness) uploadToken(t *testing.T, name string, data []byte, token string) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if token != "" {
		_ = mw.WriteField("csrf_token", token)
	}
	part, err := mw.CreateFormFile("statement", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write(data)
	_ = mw.Close()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/admin/reconcile/upload", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

var (
	mapTokenRe   = regexp.MustCompile(`action="/admin/reconcile/map/([A-Za-z0-9_-]+)"`)
	previewOfRe  = regexp.MustCompile(`name="preview_of" value="([^"]*)"`)
	importPathRe = regexp.MustCompile(`/admin/reconcile/imports/(\d+)`)
)

// mapForm is the reference layout's mapping as the form posts it.
func mapForm(token, previewOf string) url.Values {
	return url.Values{
		"csrf_token":  {token},
		"preview_of":  {previewOf},
		"date":        {"1"},
		"date_layout": {"m/d/yyyy"},
		"amount":      {"5"},
		"debit":       {"-1"},
		"credit":      {"-1"},
		"incoming":    {"positive"},
		"description": {"2"},
		"account":     {"0"},
		"note":        {"3"},
		"reference":   {"4"},
		"name":        {"Credit union"},
		"action":      {"save"},
	}
}

// importReference uploads the reference file as a new layout and saves the
// guessed mapping, returning the import page.
func (h *harness) importReference(t *testing.T) (string, string) {
	// Returns the import page's path and body.
	t.Helper()
	resp, body := h.upload(t, "TransactionHistory.csv", referenceCSV(t))
	if !strings.Contains(body, "Map this layout") {
		t.Fatalf("new layout did not open the mapping screen: %s", truncate(body))
	}
	tok := mapTokenRe.FindStringSubmatch(body)
	po := previewOfRe.FindStringSubmatch(body)
	if tok == nil || po == nil || po[1] == "" {
		t.Fatalf("mapping screen lacks its token or preview: %s", truncate(body))
	}
	mapPath := resp.Request.URL.Path
	resp, body = h.post("/admin/reconcile/map/"+tok[1], mapForm(h.csrfToken(mapPath), po[1]))
	if !importPathRe.MatchString(resp.Request.URL.Path) {
		t.Fatalf("save did not land on the import page: %s", resp.Request.URL.Path)
	}
	return resp.Request.URL.Path, body
}

func TestImportNewLayoutThenKnown(t *testing.T) {
	h := reconcileReady(t)
	logs := h.captureLog()

	resp, body := h.upload(t, "TransactionHistory.csv", referenceCSV(t))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Map this layout") {
		t.Fatalf("status %d: %s", resp.StatusCode, truncate(body))
	}
	// The guess is prefilled by name, and its preview is already on screen:
	// row 2 incoming, row 3 outgoing.
	for _, want := range []string{
		`<option value="1" selected>2. Date</option>`,
		`<option value="5" selected>6. Amount</option>`,
		`<option value="3" selected>4. Note</option>`,
		`<option value="4" selected>5. Check #</option>`,
		"Row 2", "$150.00", "out, dropped", "Save layout and import",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mapping screen lacks %q", want)
		}
	}
	// Nothing is stored until the mapping is saved.
	if fs, _ := h.db.ListBankFormats(t.Context()); len(fs) != 0 {
		t.Fatalf("a layout was saved before mapping: %+v", fs)
	}

	tok := mapTokenRe.FindStringSubmatch(body)[1]
	po := previewOfRe.FindStringSubmatch(body)[1]
	_, body = h.post("/admin/reconcile/map/"+tok, mapForm(h.csrfToken(resp.Request.URL.Path), po))
	if !strings.Contains(body, "Imported: 8 new, 2 outgoing dropped.") {
		t.Fatalf("import summary missing: %s", truncate(body))
	}

	fs, _ := h.db.ListBankFormats(t.Context())
	if len(fs) != 1 || fs[0].Name != "Credit union" || fs[0].Mapping.Note != 3 {
		t.Fatalf("saved layouts = %+v", fs)
	}
	imps, _ := h.db.ListBankImports(t.Context(), 10)
	if len(imps) != 1 || imps[0].Kept != 8 || imps[0].Outgoing != 2 || imps[0].Refused != 0 ||
		imps[0].FileName != "TransactionHistory.csv" || imps[0].FirstDate != "2026-09-01" ||
		imps[0].LastDate != "2026-09-30" || len(imps[0].FileSHA256) != 64 {
		t.Fatalf("import = %+v", imps)
	}

	// The same header again: imported straight away, nothing new.
	_, body = h.upload(t, "TransactionHistory (1).csv", referenceCSV(t))
	if strings.Contains(body, "Map this layout") {
		t.Fatal("a known layout asked for a mapping again")
	}
	if !strings.Contains(body, "Imported: 0 new, 8 already imported, 2 outgoing dropped.") {
		t.Errorf("re-import summary: %s", truncate(body))
	}

	// An overlapping file: the last three lines of the first, plus one new.
	overlap := "Account,Date,Description,Note,Check #,Amount,Balance\r\n" +
		"PRIMARY SHARE,9/8/2026,\"Check Deposit\",,1042,$75.25,\"$4,464.30\"\r\n" +
		"PRIMARY SHARE,9/9/2026,\"Withdrawal ACH\nRENT CO\",,,\"($1,500.00)\",\"$2,964.30\"\r\n" +
		"PRIMARY SHARE,9/30/2026,\"Deposit Dividend\nANNUAL PERCENTAGE YIELD EARNED 0.05%\",,,$0.12,\"$2,964.42\"\r\n" +
		"PRIMARY SHARE,10/2/2026,\"Deposit Transfer\nFROM SAM PAYEE X1234\",,,$50.00,\"$3,014.42\"\r\n"
	_, body = h.upload(t, "October.csv", []byte(overlap))
	if !strings.Contains(body, "Imported: 1 new, 2 already imported, 1 outgoing dropped.") {
		t.Errorf("overlap summary: %s", truncate(body))
	}

	var n int
	lines := 0
	imps, _ = h.db.ListBankImports(t.Context(), 10)
	for _, i := range imps {
		ls, _ := h.db.ListBankLines(t.Context(), i.ID)
		lines += len(ls)
		n++
	}
	if n != 3 || lines != 9 {
		t.Errorf("%d imports holding %d lines, want 3 and 9", n, lines)
	}
	for _, want := range []string{"bank layout saved", "bank statement imported", "kept=8", "user_id=1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q", want)
		}
	}
}

// The import page shows each kept line with its row number, the description
// on two lines, the account, the note verbatim, and the original row.
func TestImportPageShowsLines(t *testing.T) {
	h := reconcileReady(t)
	_, body := h.importReference(t)

	for _, want := range []string{
		"TransactionHistory.csv", "Credit union",
		"8 new lines kept", "2 outgoing (or zero), counted and dropped",
		"Row 2", "Sep 1, 2026", "$150.00", "from Sep 1, 2026 to Sep 30, 2026",
		"Deposit Transfer\nFROM SAM PAYEE X1234",
		"PRIMARY SHARE", "SECONDARY SAVINGS", "Ref 1042",
		"Bank note:", "Phone plan, September\nsplit with Ann",
		"Original row",
		"PRIMARY SHARE,9/1/2026,&#34;Deposit Transfer\nFROM SAM PAYEE X1234&#34;,,,$150.00,&#34;$3,239.01&#34;",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("import page lacks %q", want)
		}
	}
	// Outgoing lines were never stored, so never shown.
	for _, gone := range []string{"GROCERY MART", "RENT CO"} {
		if strings.Contains(body, gone) {
			t.Errorf("outgoing line %q appears on the import page", gone)
		}
	}
}

// A save is accepted only for the mapping that was previewed, and only with a
// name; neither failure saves anything.
func TestMapSaveRequiresPreviewAndName(t *testing.T) {
	h := reconcileReady(t)
	resp, body := h.upload(t, "TransactionHistory.csv", referenceCSV(t))
	po := previewOfRe.FindStringSubmatch(body)[1]
	mapPath := resp.Request.URL.Path

	// Change a column after the preview.
	f := mapForm(h.csrfToken(mapPath), po)
	f.Set("note", "-1")
	r, body := h.post(mapPath, f)
	if r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "changed since the preview") {
		t.Errorf("changed mapping: %d %s", r.StatusCode, truncate(body))
	}
	// The re-rendered screen previews the new choice, so saving now works.
	po2 := previewOfRe.FindStringSubmatch(body)[1]
	if po2 == po {
		t.Error("preview did not follow the changed choice")
	}

	// No name.
	f = mapForm(h.csrfToken(mapPath), po)
	f.Set("name", "  ")
	if r, body := h.post(mapPath, f); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Give the layout a name") {
		t.Errorf("no name: %d", r.StatusCode)
	}
	// An incomplete mapping cannot be saved and shows why.
	f = mapForm(h.csrfToken(mapPath), po)
	f.Set("amount", "-1")
	if r, body := h.post(mapPath, f); r.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "amount") {
		t.Errorf("incomplete mapping: %d", r.StatusCode)
	}
	// A column that is not a number.
	f = mapForm(h.csrfToken(mapPath), po)
	f.Set("date", "x")
	if r, _ := h.post(mapPath, f); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("non-numeric column: %d", r.StatusCode)
	}
	// Preview action never saves.
	f = mapForm(h.csrfToken(mapPath), po)
	f.Set("action", "preview")
	if r, _ := h.post(mapPath, f); r.StatusCode != http.StatusOK {
		t.Errorf("preview: %d", r.StatusCode)
	}

	if fs, _ := h.db.ListBankFormats(t.Context()); len(fs) != 0 {
		t.Errorf("a refused save stored a layout: %+v", fs)
	}
	if imps, _ := h.db.ListBankImports(t.Context(), 10); len(imps) != 0 {
		t.Errorf("a refused save imported: %+v", imps)
	}
}

// A pending upload belongs to whoever uploaded it, and is gone once used.
func TestPendingUploadIsPrivateAndSingleUse(t *testing.T) {
	h := reconcileReady(t)
	resp, body := h.upload(t, "TransactionHistory.csv", referenceCSV(t))
	tok := mapTokenRe.FindStringSubmatch(body)[1]
	po := previewOfRe.FindStringSubmatch(body)[1]
	mapPath := resp.Request.URL.Path

	// Another holder of the permission cannot open or use it.
	ann := h.addUser("ann@example.com", "Ann Admin", true)
	h.setReconcile(ann.ID, true)
	other := h.newClient()
	other.loginAs("ann@example.com", "a-long-enough-password")
	if _, b := other.get(mapPath); !strings.Contains(b, "expired or was already imported") {
		t.Errorf("another account opened the upload: %s", truncate(b))
	}
	other.post(mapPath, mapForm(other.csrfToken("/admin/reconcile"), po))
	if fs, _ := h.db.ListBankFormats(t.Context()); len(fs) != 0 {
		t.Fatal("another account saved someone else's upload")
	}

	// A made-up token.
	if _, b := h.get("/admin/reconcile/map/not-a-real-token"); !strings.Contains(b, "expired") {
		t.Error("an unknown token did not say so")
	}

	// Used once, then gone.
	h.post(mapPath, mapForm(h.csrfToken(mapPath), po))
	if _, b := h.post(mapPath, mapForm(h.csrfToken("/admin/reconcile"), po)); !strings.Contains(b, "expired or was already imported") {
		t.Errorf("token reusable after import: %s", truncate(b))
	}
	if imps, _ := h.db.ListBankImports(t.Context(), 10); len(imps) != 1 {
		t.Errorf("%d imports, want 1", len(imps))
	}
	_ = tok
}

// Two people mapping the same new layout: the second save finds the first's
// layout and imports with it, rather than failing or making a second.
func TestMapSaveWhenLayoutSavedMeanwhile(t *testing.T) {
	h := reconcileReady(t)
	resp, body := h.upload(t, "TransactionHistory.csv", referenceCSV(t))
	po := previewOfRe.FindStringSubmatch(body)[1]
	mapPath := resp.Request.URL.Path

	f, _ := bankcsv.Read(referenceCSV(t))
	if _, err := h.db.CreateBankFormat(t.Context(), store.BankFormat{
		Name: "Saved first", Signature: f.Signature, HeaderText: "x",
		Mapping: bankcsv.Guess(f), CreatedBy: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_, body = h.post(mapPath, mapForm(h.csrfToken(mapPath), po))
	if !strings.Contains(body, "Imported: 8 new") || !strings.Contains(body, "Saved first") {
		t.Errorf("save after a concurrent save: %s", truncate(body))
	}
	if fs, _ := h.db.ListBankFormats(t.Context()); len(fs) != 1 {
		t.Errorf("%d layouts, want 1", len(fs))
	}
}

func TestUploadRefusals(t *testing.T) {
	h := reconcileReady(t)
	cases := map[string]struct {
		data []byte
		want string
	}{
		"empty":       {[]byte(""), "the file is empty"},
		"stray quote": {[]byte("Date,Description,Amount\n9/1/2026,Bad \"q,5\n"), "not valid CSV"},
		"binary":      {[]byte("\x89PNG\r\n\x1a\n\x00\x00"), "binary data"},
		"one column":  {[]byte("Hello\nworld\n"), "header"},
	}
	for name, c := range cases {
		_, body := h.upload(t, "x.csv", c.data)
		if !strings.Contains(body, "That file was not imported") || !strings.Contains(body, c.want) {
			t.Errorf("%s: %s", name, truncate(body))
		}
	}
	// Without a CSRF token: refused, nothing held or stored.
	_, body := h.uploadToken(t, "x.csv", referenceCSV(t), "")
	if strings.Contains(body, "Map this layout") || !strings.Contains(body, "session expired") {
		t.Errorf("no token: %s", truncate(body))
	}
	if imps, _ := h.db.ListBankImports(t.Context(), 10); len(imps) != 0 {
		t.Error("a refused upload imported something")
	}
}

// Oversized uploads are cut off near the 5 MB limit, not read whole.
func TestUploadBodyIsBounded(t *testing.T) {
	h := reconcileReady(t)
	const size = 64 << 20
	if got := h.postHuge(t, "/admin/reconcile/upload", "/admin/reconcile", "statement", size); got > 24<<20 {
		t.Errorf("server read %d MB of a %d MB upload", got>>20, size>>20)
	}
	if imps, _ := h.db.ListBankImports(t.Context(), 10); len(imps) != 0 {
		t.Error("an oversized upload imported something")
	}
}

func TestUploadIsRateLimited(t *testing.T) {
	h := reconcileReady(t)
	small := []byte("Date,Description,Amount\n9/1/2026,X,5\n")
	for i := 0; i < reconcileUploadLimit; i++ {
		if _, body := h.upload(t, "x.csv", small); strings.Contains(body, "Too many uploads") {
			t.Fatalf("limited after %d uploads", i)
		}
	}
	if _, body := h.upload(t, "x.csv", small); !strings.Contains(body, "Too many uploads") {
		t.Error("upload past the limit was not refused")
	}
}

// Every import route answers 403 to an administrator without the permission
// and 404 to anyone else, and the upload stores nothing for them.
func TestImportRoutesAreGuarded(t *testing.T) {
	h := reconcileReady(t)
	importPath, _ := h.importReference(t)
	resp, body := h.upload(t, "new.csv", []byte("When,What,How much\n9/1/2026,X,5\n"))
	mapPath := resp.Request.URL.Path
	if !strings.Contains(body, "Map this layout") {
		t.Fatal("no mapping screen")
	}

	h.setReconcile(1, false)
	for _, p := range []string{"/admin/reconcile", importPath, mapPath} {
		if r, _ := h.get(p); r.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s without permission: %d, want 403", p, r.StatusCode)
		}
	}
	if r, _ := h.uploadToken(t, "x.csv", referenceCSV(t), h.csrfToken("/")); r.StatusCode != http.StatusForbidden {
		t.Errorf("upload without permission: %d", r.StatusCode)
	}

	h.addUser("sam@example.com", "Sam", false)
	h.loginAs("sam@example.com", "a-long-enough-password")
	for _, p := range []string{"/admin/reconcile", importPath, mapPath} {
		if r, _ := h.get(p); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s as non-admin: %d, want 404", p, r.StatusCode)
		}
	}
	if imps, _ := h.db.ListBankImports(t.Context(), 10); len(imps) != 1 {
		t.Errorf("%d imports, want only the first", len(imps))
	}
}

// Bank text is untrusted: it is shown escaped everywhere, and the uploaded
// file name is reduced to a harmless base name.
func TestImportEscapesBankTextAndFileName(t *testing.T) {
	h := reconcileReady(t)
	csv := "Date,Description,Note,Amount\n" +
		"9/1/2026,\"<script>alert(1)</script>\",\"<img src=x onerror=alert(2)>\",5.00\n" +
		"9/2/2026,\"=HYPERLINK(\"\"http://evil\"\")\",,6.00\n"
	resp, body := h.upload(t, "../../etc/<b>passwd.csv", []byte(csv))
	po := previewOfRe.FindStringSubmatch(body)
	if po == nil {
		t.Fatalf("no mapping screen: %s", truncate(body))
	}
	for _, raw := range []string{"<script>alert(1)", "<img src=x", "<b>passwd"} {
		if strings.Contains(body, raw) {
			t.Errorf("mapping screen renders %q unescaped", raw)
		}
	}
	mapPath := resp.Request.URL.Path
	form := url.Values{
		"csrf_token": {h.csrfToken(mapPath)}, "preview_of": {po[1]},
		"date": {"0"}, "date_layout": {"m/d/yyyy"}, "amount": {"3"}, "debit": {"-1"}, "credit": {"-1"},
		"incoming": {"positive"}, "description": {"1"}, "account": {"-1"}, "note": {"2"},
		"reference": {"-1"}, "name": {"<i>Bank</i>"}, "action": {"save"},
	}
	_, body = h.post(mapPath, form)
	if !strings.Contains(body, "Imported: 2 new") {
		t.Fatalf("import: %s", truncate(body))
	}
	for _, raw := range []string{"<script>alert(1)", "<img src=x", "<b>passwd", "<i>Bank"} {
		if strings.Contains(body, raw) {
			t.Errorf("import page renders %q unescaped", raw)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("description not shown escaped")
	}
	imps, _ := h.db.ListBankImports(t.Context(), 1)
	if imps[0].FileName != "<b>passwd.csv" {
		t.Errorf("stored file name = %q, want the base name only", imps[0].FileName)
	}
	// A formula-looking cell is kept as text; nothing evaluates it.
	ls, _ := h.db.ListBankLines(t.Context(), imps[0].ID)
	if ls[1].Description != `=HYPERLINK("http://evil")` {
		t.Errorf("formula cell = %q", ls[1].Description)
	}
}

func TestImportPageUnknownIs404(t *testing.T) {
	h := reconcileReady(t)
	for _, p := range []string{"/admin/reconcile/imports/999", "/admin/reconcile/imports/abc", "/admin/reconcile/imports/0"} {
		if r, _ := h.get(p); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d", p, r.StatusCode)
		}
	}
}

func TestCleanFileName(t *testing.T) {
	cases := map[string]string{
		"TransactionHistory.csv":  "TransactionHistory.csv",
		"C:\\Users\\me\\bank.csv": "bank.csv",
		"../../etc/passwd":        "passwd",
		"a\x00b\r\nc.csv":         "abc.csv",
		"":                        "statement.csv",
		"/":                       "statement.csv",
		strings.Repeat("é", 300):  strings.Repeat("é", 200),
		"  spaced name .csv  ":    "spaced name .csv",
	}
	for in, want := range cases {
		if got := cleanFileName(in); got != want {
			t.Errorf("cleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPendingUploads(t *testing.T) {
	p := newPendingUploads()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	a, _ := p.put(1, "a.csv", []byte("a"), "sha")
	if _, ok := p.get(a, 1); !ok {
		t.Fatal("own upload not found")
	}
	if _, ok := p.get(a, 2); ok {
		t.Error("another account found it")
	}
	// One per account: a second upload replaces the first.
	b, _ := p.put(1, "b.csv", []byte("b"), "sha")
	if _, ok := p.get(a, 1); ok {
		t.Error("the replaced upload is still there")
	}
	if a == b || len(a) < 40 {
		t.Errorf("tokens %q %q are not distinct and long", a, b)
	}
	// Expiry.
	now = now.Add(pendingTTL + time.Second)
	if _, ok := p.get(b, 1); ok {
		t.Error("an expired upload was returned")
	}
	// The cap evicts the oldest.
	var toks []string
	for i := int64(0); i < maxPending+2; i++ {
		now = now.Add(time.Second)
		tk, _ := p.put(100+i, "x", nil, "")
		toks = append(toks, tk)
	}
	if len(p.entries) != maxPending {
		t.Errorf("%d entries, want the cap %d", len(p.entries), maxPending)
	}
	if _, ok := p.get(toks[0], 100); ok {
		t.Error("the oldest survived the cap")
	}
	if _, ok := p.get(toks[len(toks)-1], 100+int64(len(toks)-1)); !ok {
		t.Error("the newest was evicted")
	}
}

// A Windows-1252 export imports with its accented text intact.
func TestImportWindows1252(t *testing.T) {
	h := reconcileReady(t)
	csv := []byte("Date,Description,Amount\n9/1/2026,Caf\xe9 L\x92Ours,5.00\n")
	resp, body := h.upload(t, "w.csv", csv)
	po := previewOfRe.FindStringSubmatch(body)
	if po == nil {
		t.Fatalf("no mapping: %s", truncate(body))
	}
	form := url.Values{
		"csrf_token": {h.csrfToken(resp.Request.URL.Path)}, "preview_of": {po[1]},
		"date": {"0"}, "date_layout": {"m/d/yyyy"}, "amount": {"2"}, "debit": {"-1"}, "credit": {"-1"},
		"incoming": {"positive"}, "description": {"1"}, "account": {"-1"}, "note": {"-1"},
		"reference": {"-1"}, "name": {"Euro"}, "action": {"save"},
	}
	_, body = h.post(resp.Request.URL.Path, form)
	if !strings.Contains(body, "Café L’Ours") {
		t.Errorf("decoded text missing: %s", truncate(body))
	}
}

// Refusal reasons quote cells; ten long ones must still fit the import's
// refused_detail (a MariaDB TEXT column), so each is bounded.
func TestRefusedDetailIsBounded(t *testing.T) {
	h := reconcileReady(t)
	var b strings.Builder
	b.WriteString("Date,Description,Amount\n")
	for i := 0; i < 12; i++ {
		b.WriteString("9/1/2026,x," + strings.Repeat("9", 15000) + "z\n")
	}
	resp, body := h.upload(t, "bad.csv", []byte(b.String()))
	po := previewOfRe.FindStringSubmatch(body)
	if po == nil {
		t.Fatalf("no mapping: %s", truncate(body))
	}
	_, body = h.post(resp.Request.URL.Path, url.Values{
		"csrf_token": {h.csrfToken(resp.Request.URL.Path)}, "preview_of": {po[1]},
		"date": {"0"}, "date_layout": {"m/d/yyyy"}, "amount": {"2"}, "debit": {"-1"}, "credit": {"-1"},
		"incoming": {"positive"}, "description": {"1"}, "account": {"-1"}, "note": {"-1"},
		"reference": {"-1"}, "name": {"Bad"}, "action": {"save"},
	})
	if !strings.Contains(body, "12 rows could not be read") {
		t.Fatalf("import: %s", truncate(body))
	}
	imps, _ := h.db.ListBankImports(t.Context(), 1)
	if n := len(imps[0].RefusedDetail); n > 4000 {
		t.Errorf("refused detail is %d bytes", n)
	}
}
