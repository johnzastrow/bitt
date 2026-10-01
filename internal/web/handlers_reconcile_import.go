package web

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/johnzastrow/bitt/internal/auth"
	"github.com/johnzastrow/bitt/internal/bankcsv"
	"github.com/johnzastrow/bitt/internal/store"
	"github.com/johnzastrow/bitt/internal/web/views"
)

// Bank statement import (RECON-02). Every handler here sits behind
// requireReconcile.

const (
	reconcileUploadLimit  = 10
	reconcileUploadWindow = time.Minute
	previewRows           = 10
	refusedDetailLines    = 10
)

// allowReconcileUpload is a fixed-window limit on uploads per account, the
// same shape as the avatar limiter: parsing a 5 MB file is the expensive
// request here.
func (s *Server) allowReconcileUpload(userID int64) bool {
	now := time.Now()
	v, _ := s.reconcileRate.LoadOrStore(userID, &rateEntry{})
	e := v.(*rateEntry)
	s.reconcileRateMu.Lock()
	defer s.reconcileRateMu.Unlock()
	if now.After(e.reset) {
		e.count, e.reset = 0, now.Add(reconcileUploadWindow)
	}
	if e.count >= reconcileUploadLimit {
		return false
	}
	e.count++
	return true
}

func (s *Server) getReconcile(w http.ResponseWriter, r *http.Request) {
	imports, err := s.store.ListBankImports(r.Context(), 20)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	formats, err := s.store.ListBankFormats(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, views.Reconcile(s.page(w, r, "Reconciliation"), views.ReconcileData{
		Imports: imports, Formats: formats,
	}))
}

// postReconcileUpload takes one CSV. A known layout imports at once; an
// unknown one is held in memory and the mapping screen opens.
func (s *Server) postReconcileUpload(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	const back = "/admin/reconcile"

	// The limit goes on before anything reads the body: CheckCSRF parses the
	// form, and with no limit in place that parse has none either.
	r.Body = http.MaxBytesReader(w, r.Body, bankcsv.MaxBytes+16<<10)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		redirectWith(w, r, back, "err", "That file is too large. The limit is 5 MB.")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	if !auth.CheckCSRF(r) {
		redirectWith(w, r, back, "err", "Your session expired. Please try again.")
		return
	}
	if !s.allowReconcileUpload(user.ID) {
		s.log.Warn("bank upload rate limited", "user_id", user.ID)
		redirectWith(w, r, back, "err", "Too many uploads just now. Try again in a minute.")
		return
	}

	file, hdr, err := r.FormFile("statement")
	if err != nil {
		redirectWith(w, r, back, "err", "Choose a CSV file to upload.")
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, bankcsv.MaxBytes+1))
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	parsed, err := bankcsv.Read(data)
	if err != nil {
		redirectWith(w, r, back, "err", refusalMessage(err))
		return
	}
	sum := sha256.Sum256(data)
	sha := hex.EncodeToString(sum[:])
	name := cleanFileName(hdr.Filename)

	format, err := s.store.BankFormatBySignature(r.Context(), parsed.Signature)
	switch {
	case errors.Is(err, store.ErrNotFound):
		token, err := s.pending.put(user.ID, name, data, sha)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		http.Redirect(w, r, "/admin/reconcile/map/"+token, http.StatusSeeOther)
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.importWith(w, r, format, parsed, name, sha)
}

// importWith applies a saved layout to a file, stores what it keeps, and
// shows the import.
func (s *Server) importWith(w http.ResponseWriter, r *http.Request, format store.BankFormat,
	parsed bankcsv.File, fileName, sha string) {
	user := userFrom(r.Context())
	res := bankcsv.Apply(parsed, format.Mapping)

	imp := store.BankImport{
		FormatID: format.ID, FileName: fileName, FileSHA256: sha, UploadedBy: user.ID,
		Outgoing: res.Outgoing, Refused: len(res.Refused),
	}
	for i, ref := range res.Refused {
		if i == refusedDetailLines {
			imp.RefusedDetail += fmt.Sprintf("... and %d more\n", len(res.Refused)-i)
			break
		}
		imp.RefusedDetail += fmt.Sprintf("Row %d %s\n", ref.Row, ref.Reason)
	}
	lines := bankLinesFrom(format.ID, res.Incoming)
	for _, l := range lines {
		if imp.FirstDate == "" || l.PostedOn < imp.FirstDate {
			imp.FirstDate = l.PostedOn
		}
		if l.PostedOn > imp.LastDate {
			imp.LastDate = l.PostedOn
		}
	}

	saved, err := s.store.SaveBankImport(r.Context(), imp, lines)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("bank statement imported",
		"import_id", saved.ID, "format_id", format.ID, "user_id", user.ID,
		"kept", saved.Kept, "duplicates", saved.Duplicates, "outgoing", saved.Outgoing,
		"ignored", saved.Ignored, "refused", saved.Refused)
	redirectWith(w, r, "/admin/reconcile/imports/"+strconv.FormatInt(saved.ID, 10), "ok", importSummary(saved))
}

// importSummary is the one-line result shown after an upload.
func importSummary(i store.BankImport) string {
	parts := []string{fmt.Sprintf("%d new", i.Kept)}
	if i.Duplicates > 0 {
		parts = append(parts, fmt.Sprintf("%d already imported", i.Duplicates))
	}
	if i.Ignored > 0 {
		parts = append(parts, fmt.Sprintf("%d ignored by rule", i.Ignored))
	}
	parts = append(parts, fmt.Sprintf("%d outgoing dropped", i.Outgoing))
	if i.Refused > 0 {
		parts = append(parts, fmt.Sprintf("%d rows could not be read", i.Refused))
	}
	return "Imported: " + strings.Join(parts, ", ") + "."
}

func bankLinesFrom(formatID int64, parsed []bankcsv.Line) []store.BankLine {
	fps := bankcsv.Fingerprints(formatID, parsed)
	out := make([]store.BankLine, len(parsed))
	for i, p := range parsed {
		out[i] = store.BankLine{
			Row: p.Row, Raw: p.Raw, Account: p.Account,
			PostedOn: p.PostedOn.Format("2006-01-02"), Amount: p.Amount,
			Description: p.Description, Note: p.Note, Reference: p.Reference,
			Fingerprint: fps[i], State: store.BankLineOpen,
		}
	}
	return out
}

// refusalMessage turns a refused file into the message shown.
func refusalMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), bankcsv.ErrRefused.Error()+": ")
	if !errors.Is(err, bankcsv.ErrRefused) {
		msg = "the file could not be read"
	}
	return "That file was not imported: " + msg + "."
}

// cleanFileName keeps only the base name a browser sent, without control
// characters, bounded. It is display text, never a path on this server.
func cleanFileName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		name = "statement.csv"
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name
}

// ---------------------------------------------------------------------------
// Mapping a new layout
// ---------------------------------------------------------------------------

func (s *Server) pendingFor(w http.ResponseWriter, r *http.Request) (*pendingUpload, bankcsv.File, bool) {
	up, ok := s.pending.get(r.PathValue("token"), userFrom(r.Context()).ID)
	if !ok {
		redirectWith(w, r, "/admin/reconcile", "err",
			"That upload has expired or was already imported. Upload the file again.")
		return nil, bankcsv.File{}, false
	}
	parsed, err := bankcsv.Read(up.data)
	if err != nil { // it read once already; this would be a bug
		s.serverError(w, r, err)
		return nil, bankcsv.File{}, false
	}
	return up, parsed, true
}

// getReconcileMap shows the mapping screen for an unknown layout, starting
// from a guess, with the guess's preview already shown.
func (s *Server) getReconcileMap(w http.ResponseWriter, r *http.Request) {
	up, parsed, ok := s.pendingFor(w, r)
	if !ok {
		return
	}
	m := bankcsv.Guess(parsed)
	name := strings.TrimSuffix(up.fileName, path.Ext(up.fileName))
	s.renderMap(w, r, http.StatusOK, up, parsed, m, name, "")
}

func (s *Server) renderMap(w http.ResponseWriter, r *http.Request, status int, up *pendingUpload,
	parsed bankcsv.File, m bankcsv.Mapping, name, problem string) {
	d := views.ReconcileMapData{
		Token: up.token, FileName: up.fileName, Header: parsed.Header, Mapping: m, Name: name,
		Problem: problem, DateLayouts: bankcsv.DateLayouts, Rows: len(parsed.Records),
	}
	if err := m.Validate(len(parsed.Header)); err != nil {
		if d.Problem == "" {
			d.Problem = "Before saving: " + err.Error() + "."
		}
	} else {
		d.Preview = bankcsv.Preview(parsed, m, previewRows)
		d.PreviewOf = mappingKey(m)
	}
	s.render(w, r, status, views.ReconcileMap(s.page(w, r, "Map this layout"), d))
}

// postReconcileMap previews a mapping, or saves it and imports the file.
//
// Save is accepted only for the exact mapping whose preview was on screen
// (PreviewOf), so nothing is saved that a person has not seen applied to the
// file's first rows.
func (s *Server) postReconcileMap(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if err := r.ParseForm(); err != nil || !auth.CheckCSRF(r) {
		redirectWith(w, r, "/admin/reconcile", "err", "Your session expired. Please try again.")
		return
	}
	up, parsed, ok := s.pendingFor(w, r)
	if !ok {
		return
	}

	m, err := mappingFromForm(r)
	name := strings.TrimSpace(r.PostFormValue("name"))
	if err != nil {
		s.renderMap(w, r, http.StatusUnprocessableEntity, up, parsed, bankcsv.Guess(parsed), name, err.Error())
		return
	}
	if r.PostFormValue("action") != "save" {
		s.renderMap(w, r, http.StatusOK, up, parsed, m, name, "")
		return
	}

	if err := m.Validate(len(parsed.Header)); err != nil {
		s.renderMap(w, r, http.StatusUnprocessableEntity, up, parsed, m, name, "Before saving: "+err.Error()+".")
		return
	}
	if r.PostFormValue("preview_of") != mappingKey(m) {
		s.renderMap(w, r, http.StatusUnprocessableEntity, up, parsed, m, name,
			"The choices changed since the preview below. Check it, then save.")
		return
	}
	if name == "" || len([]rune(name)) > 120 {
		s.renderMap(w, r, http.StatusUnprocessableEntity, up, parsed, m, name,
			"Give the layout a name of up to 120 characters, such as the bank and account.")
		return
	}

	format, err := s.store.CreateBankFormat(r.Context(), store.BankFormat{
		Name: name, Signature: parsed.Signature, HeaderText: headerText(parsed),
		Mapping: m, CreatedBy: user.ID,
	})
	if errors.Is(err, store.ErrConflict) {
		// Someone saved this layout while this screen was open; theirs stands.
		format, err = s.store.BankFormatBySignature(r.Context(), parsed.Signature)
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.log.Info("bank layout saved", "format_id", format.ID, "user_id", user.ID)
	s.pending.drop(up.token)
	s.importWith(w, r, format, parsed, up.fileName, up.sha256)
}

// headerText is the header row as text, for showing which layout a format is.
func headerText(f bankcsv.File) string {
	return strings.Join(f.Header, string(f.Separator))
}

// mappingKey is a canonical text form of a mapping, to tie a save to the
// preview that was shown.
func mappingKey(m bankcsv.Mapping) string {
	return fmt.Sprintf("%d|%s|%d|%d|%d|%t|%t|%d|%d|%d|%d", m.Date, m.DateLayout, m.Amount,
		m.Debit, m.Credit, m.IncomingNegative, m.DecimalComma, m.Description, m.Account, m.Note, m.Reference)
}

// mappingFromForm reads the column choices. Range checks against the file
// are Validate's; this only refuses what is not a number.
func mappingFromForm(r *http.Request) (bankcsv.Mapping, error) {
	col := func(field string) (int, error) {
		v := strings.TrimSpace(r.PostFormValue(field))
		if v == "" {
			return bankcsv.None, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < bankcsv.None || n > 10_000 {
			return 0, fmt.Errorf("could not read the %s choice", field)
		}
		return n, nil
	}
	var (
		m   bankcsv.Mapping
		err error
	)
	for _, f := range []struct {
		name string
		dst  *int
	}{
		{"date", &m.Date}, {"amount", &m.Amount}, {"debit", &m.Debit}, {"credit", &m.Credit},
		{"description", &m.Description}, {"account", &m.Account}, {"note", &m.Note},
		{"reference", &m.Reference},
	} {
		if *f.dst, err = col(f.name); err != nil {
			return bankcsv.Mapping{}, err
		}
	}
	m.DateLayout = r.PostFormValue("date_layout")
	m.IncomingNegative = r.PostFormValue("incoming") == "negative"
	m.DecimalComma = r.PostFormValue("decimal_comma") == "1"
	return m, nil
}

// getReconcileImport shows one import: its counts and its kept lines.
func (s *Server) getReconcileImport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		http.NotFound(w, r)
		return
	}
	imp, err := s.store.GetBankImport(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	lines, err := s.store.ListBankLines(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, views.ReconcileImport(s.page(w, r, "Import"), imp, lines))
}
