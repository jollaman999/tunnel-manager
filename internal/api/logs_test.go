package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// writeLog puts a file in a directory of this test and hands back its path. The
// lines are written as they would be in the log, with the trailing newline the
// logger ends every line with.
func writeLog(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "tunnel-manager.log")

	body := ""
	for _, line := range lines {
		body += line + "\n"
	}

	err := os.WriteFile(path, []byte(body), 0644)
	if err != nil {
		t.Fatalf("failed to write the log file: %v", err)
	}

	return path
}

// logsRequest runs one GET against the handler and hands back what it wrote.
func logsRequest(t *testing.T, path string, query string) *httptest.ResponseRecorder {
	t.Helper()

	target := "/api/logs"
	if query != "" {
		target += "?" + query
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := NewLogsHandler(zap.NewNop(), path, nil, nil).GetLogs(c)
	if err != nil {
		t.Fatalf("the handler returned error: %v", err)
	}

	return rec
}

// decodeLogs reads an answer that was meant to succeed.
func decodeLogs(t *testing.T, rec *httptest.ResponseRecorder) logsAnswer {
	t.Helper()

	var resp struct {
		Success bool       `json:"success"`
		Data    logsAnswer `json:"data"`
		Error   string     `json:"error"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if !resp.Success {
		t.Fatalf("success = false, body: %s", rec.Body.String())
	}

	return resp.Data
}

// decodeRefusal reads the message of an answer that was meant to fail. The
// message is what the screen puts in front of the operator, so it is what is
// checked rather than only the status code.
func decodeRefusal(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var resp struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if resp.Success {
		t.Fatalf("success = true on an answer that was meant to fail, body: %s", rec.Body.String())
	}

	// A refusal that carries a list as well would let a screen draw an empty
	// log beside the reason it is empty, which is the confusion this answer
	// exists to avoid.
	if len(resp.Data) != 0 {
		t.Fatalf("the refusal carries data: %s", rec.Body.String())
	}

	return resp.Error
}

// rawOf is what the lines of an answer say, in the order they arrived.
func rawOf(lines []logLine) []string {
	raw := make([]string, 0, len(lines))

	for _, line := range lines {
		raw = append(raw, line.Raw)
	}

	return raw
}

// TestTheLastLinesAreWhatComesBack is the call the screen is filled from. The
// order is the order of the file, oldest first, because the screen is the side
// that decides which end it draws first.
func TestTheLastLinesAreWhatComesBack(t *testing.T) {
	path := writeLog(t, "one", "two", "three", "four", "five")

	rec := logsRequest(t, path, "lines=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	answer := decodeLogs(t, rec)

	got := strings.Join(rawOf(answer.Lines), ",")
	if got != "four,five" {
		t.Fatalf("lines = %q, want \"four,five\"", got)
	}

	if answer.Path != path {
		t.Errorf("path = %q, want %q", answer.Path, path)
	}
}

// TestAFileShorterThanTheCountComesBackWhole covers the log of an installation
// that has just started.
func TestAFileShorterThanTheCountComesBackWhole(t *testing.T) {
	path := writeLog(t, "one", "two")

	answer := decodeLogs(t, logsRequest(t, path, "lines=500"))

	got := strings.Join(rawOf(answer.Lines), ",")
	if got != "one,two" {
		t.Fatalf("lines = %q, want \"one,two\"", got)
	}
}

// TestTheLastLineWithoutANewlineIsNotLost is what the end of the file looks
// like while a line is being written, and after a crash.
func TestTheLastLineWithoutANewlineIsNotLost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.log")

	err := os.WriteFile(path, []byte("one\ntwo\nthree"), 0644)
	if err != nil {
		t.Fatalf("failed to write the log file: %v", err)
	}

	answer := decodeLogs(t, logsRequest(t, path, "lines=2"))

	got := strings.Join(rawOf(answer.Lines), ",")
	if got != "two,three" {
		t.Fatalf("lines = %q, want \"two,three\"", got)
	}
}

// TestAnEmptyFileIsAnEmptyList tells a log with nothing in it from a log that
// cannot be read: this one succeeds.
func TestAnEmptyFileIsAnEmptyList(t *testing.T) {
	path := writeLog(t)

	rec := logsRequest(t, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	answer := decodeLogs(t, rec)
	if len(answer.Lines) != 0 {
		t.Fatalf("lines = %v, want none", rawOf(answer.Lines))
	}
}

// TestTheCountIsCutDownToTheBound is the bound that keeps one request from
// pulling an unbounded number of lines into memory.
func TestTheCountIsCutDownToTheBound(t *testing.T) {
	lines := make([]string, 0, logsMaxLines+10)
	for i := 0; i < logsMaxLines+10; i++ {
		lines = append(lines, "line "+strconv.Itoa(i))
	}

	path := writeLog(t, lines...)

	answer := decodeLogs(t, logsRequest(t, path, "lines=100000"))

	if answer.Requested != logsMaxLines {
		t.Errorf("requested = %d, want %d", answer.Requested, logsMaxLines)
	}

	if answer.MaxLines != logsMaxLines {
		t.Errorf("max_lines = %d, want %d", answer.MaxLines, logsMaxLines)
	}

	if len(answer.Lines) != logsMaxLines {
		t.Fatalf("lines = %d, want %d", len(answer.Lines), logsMaxLines)
	}

	// What comes back is still the end of the file, not the beginning of it.
	last := answer.Lines[len(answer.Lines)-1].Raw
	if last != "line "+strconv.Itoa(logsMaxLines+9) {
		t.Fatalf("the last line is %q, want the last line of the file", last)
	}
}

// TestACountThatIsNotAPositiveNumberIsRefused keeps a screen that worked a
// number out wrong from being answered with the default as though it had asked
// for it.
func TestACountThatIsNotAPositiveNumberIsRefused(t *testing.T) {
	path := writeLog(t, "one")

	for _, query := range []string{"lines=abc", "lines=0", "lines=-5", "lines=1.5"} {
		rec := logsRequest(t, path, query)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GET ?%s = %d, want %d, body: %s", query, rec.Code,
				http.StatusBadRequest, rec.Body.String())
		}

		if message := decodeRefusal(t, rec); !strings.Contains(message, "lines") {
			t.Errorf("GET ?%s was refused with %q, which does not name the parameter",
				query, message)
		}
	}
}

// TestNoCountIsTheDefault covers the call the screen makes when it names none.
func TestNoCountIsTheDefault(t *testing.T) {
	answer := decodeLogs(t, logsRequest(t, writeLog(t, "one"), ""))

	if answer.Requested != logsDefaultLines {
		t.Fatalf("requested = %d, want %d", answer.Requested, logsDefaultLines)
	}
}

// TestAMissingFileSaysWhy is the answer a screen has to be able to tell from an
// empty log. A server that could not open its log file writes to the console
// only, and the operator reading this screen cannot see that console.
func TestAMissingFileSaysWhy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "tunnel-manager.log")

	rec := logsRequest(t, path, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	message := decodeRefusal(t, rec)

	// The path is in the message because it is the first thing to check, and
	// the screen has no other way of knowing it.
	if !strings.Contains(message, path) {
		t.Errorf("the refusal does not name the file: %q", message)
	}

	if !strings.Contains(message, "console") {
		t.Errorf("the refusal does not say where the logs went instead: %q", message)
	}
}

// TestNoConfiguredFileSaysWhy covers the settings naming no log file at all.
func TestNoConfiguredFileSaysWhy(t *testing.T) {
	rec := logsRequest(t, "", "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}

	if message := decodeRefusal(t, rec); !strings.Contains(message, "console") {
		t.Errorf("the refusal does not say where the logs went instead: %q", message)
	}
}

// TestADirectoryIsRefusedAsSuch keeps a path that names a directory from being
// read as a file full of nonsense.
func TestADirectoryIsRefusedAsSuch(t *testing.T) {
	rec := logsRequest(t, t.TempDir(), "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code,
			http.StatusInternalServerError, rec.Body.String())
	}

	if message := decodeRefusal(t, rec); !strings.Contains(message, "directory") {
		t.Errorf("the refusal does not say what is wrong: %q", message)
	}
}

// TestAJSONLineIsSplitIntoItsFields covers the format a fresh installation logs
// in. The line is the one zap writes with the encoder this repository builds.
func TestAJSONLineIsSplitIntoItsFields(t *testing.T) {
	raw := `{"level":"warn","timestamp":"2026-09-18T10:11:12.345+0900",` +
		`"caller":"tunnel/manager.go:210","msg":"failed to open the tunnel",` +
		`"host_id":7,"error":"connection refused"}`

	answer := decodeLogs(t, logsRequest(t, writeLog(t, raw), ""))

	if len(answer.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(answer.Lines))
	}

	line := answer.Lines[0]

	if !line.Parsed {
		t.Fatalf("the line was not parsed: %+v", line)
	}

	if line.Level != "warn" {
		t.Errorf("level = %q, want warn", line.Level)
	}

	if line.Time != "2026-09-18T10:11:12.345+0900" {
		t.Errorf("time = %q", line.Time)
	}

	if line.Caller != "tunnel/manager.go:210" {
		t.Errorf("caller = %q", line.Caller)
	}

	if line.Message != "failed to open the tunnel" {
		t.Errorf("message = %q", line.Message)
	}

	// The fields the line carried besides those four are handed on. They are
	// what says which host the line is about, so dropping them would leave the
	// screen showing a failure that names nothing.
	if !strings.Contains(line.Extra, `"host_id":7`) ||
		!strings.Contains(line.Extra, `"error":"connection refused"`) {
		t.Errorf("extra = %q, want the fields the line carried", line.Extra)
	}

	if line.Raw != raw {
		t.Errorf("raw = %q, want the line as it stands", line.Raw)
	}
}

// TestAConsoleLineIsSplitIntoItsFields covers the other stored format. The
// fields are separated by tabs and stand in a fixed order.
func TestAConsoleLineIsSplitIntoItsFields(t *testing.T) {
	raw := "2026-09-18T10:11:12.345+0900\tinfo\tmain.go:604\tStarting tunnel manager\t" +
		`{"version": "2.1.0"}`

	answer := decodeLogs(t, logsRequest(t, writeLog(t, raw), ""))

	line := answer.Lines[0]

	if !line.Parsed {
		t.Fatalf("the line was not parsed: %+v", line)
	}

	if line.Level != "info" {
		t.Errorf("level = %q, want info", line.Level)
	}

	if line.Time != "2026-09-18T10:11:12.345+0900" {
		t.Errorf("time = %q", line.Time)
	}

	if line.Caller != "main.go:604" {
		t.Errorf("caller = %q", line.Caller)
	}

	if line.Message != "Starting tunnel manager" {
		t.Errorf("message = %q", line.Message)
	}

	if line.Extra != `{"version": "2.1.0"}` {
		t.Errorf("extra = %q", line.Extra)
	}
}

// TestAConsoleLineWithoutACallerIsStillRead covers a line written by a logger
// that was not built with the caller, so that the message does not end up in
// the caller column.
func TestAConsoleLineWithoutACallerIsStillRead(t *testing.T) {
	raw := "2026-09-18T10:11:12.345+0900\terror\tthe database is locked"

	line := decodeLogs(t, logsRequest(t, writeLog(t, raw), "")).Lines[0]

	if !line.Parsed {
		t.Fatalf("the line was not parsed: %+v", line)
	}

	if line.Caller != "" {
		t.Errorf("caller = %q, want none", line.Caller)
	}

	if line.Message != "the database is locked" {
		t.Errorf("message = %q", line.Message)
	}
}

// TestAConsoleLevelWrittenInCapitalsIsRead covers a file written by a build
// whose encoder was configured the other way.
func TestAConsoleLevelWrittenInCapitalsIsRead(t *testing.T) {
	raw := "2026-09-18T10:11:12.345+0900\tWARN\tmain.go:10\tsomething"

	line := decodeLogs(t, logsRequest(t, writeLog(t, raw), "")).Lines[0]

	if line.Level != "warn" {
		t.Errorf("level = %q, want warn", line.Level)
	}
}

// TestALineThatParsesAsNeitherFormatIsKept is the point of the whole answer
// carrying the raw line. A line that does not look like the others is the one
// worth seeing, and dropping it would leave the screen looking orderly while
// the file holds the answer.
func TestALineThatParsesAsNeitherFormatIsKept(t *testing.T) {
	odd := "panic: runtime error: invalid memory address"

	answer := decodeLogs(t, logsRequest(t, writeLog(t, "one\ttwo", odd, "{not json at all"), ""))

	if len(answer.Lines) != 3 {
		t.Fatalf("lines = %d, want 3: %v", len(answer.Lines), rawOf(answer.Lines))
	}

	for _, line := range answer.Lines {
		if line.Parsed {
			t.Errorf("%q was read as a log line: %+v", line.Raw, line)
		}

		if line.Level != "" {
			t.Errorf("%q was given the level %q it does not carry", line.Raw, line.Level)
		}
	}

	if answer.Lines[1].Raw != odd {
		t.Errorf("raw = %q, want %q", answer.Lines[1].Raw, odd)
	}
}

// TestBothFormatsInOneFileAreRead is what a file looks like after the format
// was changed on the Settings screen: the lines written before the restart are
// in the old format and the ones after it are in the new one.
func TestBothFormatsInOneFileAreRead(t *testing.T) {
	answer := decodeLogs(t, logsRequest(t, writeLog(t,
		"2026-09-18T10:11:12.345+0900\tinfo\tmain.go:604\tbefore the restart",
		`{"level":"info","timestamp":"2026-09-18T10:12:00.000+0900","caller":"main.go:604",`+
			`"msg":"after the restart"}`), ""))

	if len(answer.Lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(answer.Lines))
	}

	for _, line := range answer.Lines {
		if !line.Parsed || line.Level != "info" {
			t.Errorf("%q was not read: %+v", line.Raw, line)
		}
	}

	if answer.Lines[0].Message != "before the restart" ||
		answer.Lines[1].Message != "after the restart" {
		t.Fatalf("messages = %q and %q", answer.Lines[0].Message, answer.Lines[1].Message)
	}
}

// TestOnlyTheEndOfALargeFileIsRead is what this handler exists for. The log is
// allowed to grow to a hundred megabytes before it rotates, and reading it
// whole would hold all of it in the process for the length of one request.
func TestOnlyTheEndOfALargeFileIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.log")

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create the log file: %v", err)
	}

	// Sixteen megabytes is past logsMaxBytes several times over, so a read that
	// walked the file would be caught here whatever the line count asked for.
	line := strings.Repeat("x", 255) + "\n"
	block := strings.Repeat(line, 4096)

	for written := 0; written < 16*1024*1024; written += len(block) {
		_, err = file.WriteString(block)
		if err != nil {
			t.Fatalf("failed to write the log file: %v", err)
		}
	}

	err = file.Close()
	if err != nil {
		t.Fatalf("failed to close the log file: %v", err)
	}

	answer := decodeLogs(t, logsRequest(t, path, "lines=200"))

	if len(answer.Lines) != 200 {
		t.Fatalf("lines = %d, want 200", len(answer.Lines))
	}

	if answer.Size < 16*1024*1024 {
		t.Fatalf("size = %d, want the size of the file", answer.Size)
	}

	// 200 lines of 256 bytes are 50KB, which is one chunk of the walk. What is
	// checked is the order of magnitude: the read is kilobytes against a file
	// of megabytes, and it does not grow with the file.
	if answer.Read > 2*logsChunkBytes {
		t.Fatalf("read = %d bytes to answer with 200 lines, want at most %d",
			answer.Read, 2*logsChunkBytes)
	}

	if answer.Capped {
		t.Errorf("capped = true, but the lines were found inside the byte limit")
	}
}

// TestTheByteLimitEndsTheWalkAndSaysSo covers the file the line count alone
// does not bound: one whose lines are enormous. The read stops at the limit and
// the answer says that the oldest line is not as far back as was asked for.
func TestTheByteLimitEndsTheWalkAndSaysSo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.log")

	// Two lines, each wider than the limit. Asking for both cannot be met
	// without reading more than one request may.
	huge := strings.Repeat("y", logsMaxBytes+1024)

	err := os.WriteFile(path, []byte(huge+"\n"+huge+"\n"), 0644)
	if err != nil {
		t.Fatalf("failed to write the log file: %v", err)
	}

	answer := decodeLogs(t, logsRequest(t, path, "lines=2"))

	if !answer.Capped {
		t.Fatalf("capped = false, want true: read %d of %d", answer.Read, answer.Size)
	}

	if answer.Read > logsMaxBytes {
		t.Fatalf("read = %d, which is past the limit of %d", answer.Read, logsMaxBytes)
	}

	// What was read is still handed over rather than being thrown away for
	// being cut at the front. An empty answer here would say the log holds
	// nothing, which is the opposite of what is going on.
	if len(answer.Lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(answer.Lines))
	}

	if length := len(answer.Lines[0].Raw); length > logsMaxBytes {
		t.Fatalf("the line is %d bytes, which is past the limit of %d", length, logsMaxBytes)
	}
}

// TestABlankLineIsNotDrawnAsARow keeps a stray empty line out of the table. zap
// writes none, so one in the file came from something else.
func TestABlankLineIsNotDrawnAsARow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.log")

	err := os.WriteFile(path, []byte("one\n\n\ntwo\n"), 0644)
	if err != nil {
		t.Fatalf("failed to write the log file: %v", err)
	}

	answer := decodeLogs(t, logsRequest(t, path, ""))

	got := strings.Join(rawOf(answer.Lines), ",")
	if got != "one,two" {
		t.Fatalf("lines = %q, want \"one,two\"", got)
	}
}

// TestALineIsNotCutAtAChunkBoundary covers the walk itself. A line that spans
// two of the steps it reads in has to come back whole, and a count that needs
// more than one step has to be met.
func TestALineIsNotCutAtAChunkBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tunnel-manager.log")

	// Each line is a tenth of a chunk, so the twenty asked for span several
	// steps of the walk and the boundaries fall inside lines.
	var body strings.Builder

	for i := 0; i < 400; i++ {
		body.WriteString(strconv.Itoa(i) + " " + strings.Repeat("z", logsChunkBytes/10) + "\n")
	}

	err := os.WriteFile(path, []byte(body.String()), 0644)
	if err != nil {
		t.Fatalf("failed to write the log file: %v", err)
	}

	answer := decodeLogs(t, logsRequest(t, path, "lines=20"))

	if len(answer.Lines) != 20 {
		t.Fatalf("lines = %d, want 20", len(answer.Lines))
	}

	for index, line := range answer.Lines {
		want := strconv.Itoa(380+index) + " " + strings.Repeat("z", logsChunkBytes/10)
		if line.Raw != want {
			t.Fatalf("line %d is %d bytes and starts with %q, want the whole line %d",
				index, len(line.Raw), firstBytes(line.Raw), 380+index)
		}
	}
}

// firstBytes is the start of a line, for a failure message that would otherwise
// be thousands of characters of filler.
func firstBytes(line string) string {
	if len(line) <= 16 {
		return line
	}

	return line[:16]
}

// clearLogsDB opens a database with the one account in it, which is what the
// password under the press is checked against.
func clearLogsDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "logsclear.db")),
		&gorm.Config{Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("failed to open the database: %v", err)
	}

	t.Cleanup(func() {
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})

	err = db.AutoMigrate(&models.User{})
	if err != nil {
		t.Fatalf("failed to migrate the database: %v", err)
	}

	hash, err := auth.HashPassword(hostKeyAccountPassword)
	if err != nil {
		t.Fatalf("failed to hash the password: %v", err)
	}

	err = db.Create(&models.User{Username: "operator", PasswordHash: hash}).Error
	if err != nil {
		t.Fatalf("failed to create the account: %v", err)
	}

	return db
}

// clearLogsCall is what one press is run with. empty stands in for the closure
// the startup hands the handler, and emptied says whether it was reached: the
// tests that refuse the press hold it to having been left alone, which is the
// half of a refusal that the status code does not say.
type clearLogsCall struct {
	path    string
	db      *gorm.DB
	body    string
	account bool
	fail    error
	emptied bool
}

func (call *clearLogsCall) run(t *testing.T) (*httptest.ResponseRecorder, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zap.DebugLevel)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/logs/clear", strings.NewReader(call.body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if call.account {
		leaveSessionOnContext(c, 1)
	}

	var empty func() error

	if call.path != "" {
		empty = func() error {
			call.emptied = true

			if call.fail != nil {
				return call.fail
			}

			return os.Truncate(call.path, 0)
		}
	}

	err := NewLogsHandler(zap.New(core), call.path, call.db, empty).ClearLogs(c)
	if err != nil {
		t.Fatalf("the handler returned error: %v", err)
	}

	return rec, logs
}

// TestEmptyingTheLogEmptiesTheFile is the press going through.
func TestEmptyingTheLogEmptiesTheFile(t *testing.T) {
	path := writeLog(t, `{"level":"info","msg":"something happened"}`)

	call := &clearLogsCall{
		path:    path,
		db:      clearLogsDB(t),
		body:    `{"password":"` + hostKeyAccountPassword + `"}`,
		account: true,
	}

	rec, _ := call.run(t)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", rec.Code, rec.Body.String())
	}

	if !call.emptied {
		t.Error("the file was not emptied")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the log file: %v", err)
	}

	if info.Size() != 0 {
		t.Errorf("the log file is %d bytes, want 0", info.Size())
	}
}

// TestEmptyingTheLogSaysWhichFileWasEmptied holds the answer to naming the file
// the server emptied. The screen must not be the side that decides where the
// log is, the same as on the read.
func TestEmptyingTheLogSaysWhichFileWasEmptied(t *testing.T) {
	path := writeLog(t, `{"level":"info","msg":"something happened"}`)

	call := &clearLogsCall{
		path:    path,
		db:      clearLogsDB(t),
		body:    `{"password":"` + hostKeyAccountPassword + `"}`,
		account: true,
	}

	rec, _ := call.run(t)

	var resp struct {
		Success bool              `json:"success"`
		Data    logsClearedAnswer `json:"data"`
	}

	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	if resp.Data.Path != path {
		t.Errorf("path = %q, want %q", resp.Data.Path, path)
	}
}

// TestEmptyingTheLogWithNoPasswordIsRefusedBeforeTheCheck holds an empty box to
// its own refusal. Answered as a password that is wrong, it would tell an
// operator who pressed with nothing typed that their password is not their
// password.
func TestEmptyingTheLogWithNoPasswordIsRefusedBeforeTheCheck(t *testing.T) {
	path := writeLog(t, `{"level":"info","msg":"something happened"}`)

	call := &clearLogsCall{
		path:    path,
		db:      clearLogsDB(t),
		body:    `{"password":""}`,
		account: true,
	}

	rec, _ := call.run(t)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", rec.Code, rec.Body.String())
	}

	if call.emptied {
		t.Error("the file was emptied by a press with no password")
	}

	message := decodeRefusal(t, rec)
	if !strings.Contains(message, "password of your account") {
		t.Errorf("the refusal reads %q, which does not name the box to fill", message)
	}
}

// TestEmptyingTheLogWithTheWrongPasswordLeavesTheFile is the refusal the screen
// reads by its code, so that it stays on the screen it is on rather than being
// sent to the login.
func TestEmptyingTheLogWithTheWrongPasswordLeavesTheFile(t *testing.T) {
	path := writeLog(t, `{"level":"info","msg":"something happened"}`)

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the log file: %v", err)
	}

	call := &clearLogsCall{
		path:    path,
		db:      clearLogsDB(t),
		body:    `{"password":"not the password"}`,
		account: true,
	}

	rec, logs := call.run(t)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body: %s", rec.Code, rec.Body.String())
	}

	if call.emptied {
		t.Error("the file was emptied by a press with the wrong password")
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat the log file: %v", err)
	}

	if after.Size() != before.Size() {
		t.Errorf("the log file is %d bytes, want the %d it was", after.Size(), before.Size())
	}

	var code struct {
		Code string `json:"error_code"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &code)
	if err != nil {
		t.Fatalf("failed to read the answer: %v, body: %s", err, rec.Body.String())
	}

	// The screen branches on this name to tell a wrong password from a session
	// that has ended. Renamed here and not there, the operator would be sent to
	// the login by a typo.
	if code.Code != string(errLogsClearPasswordWrong) {
		t.Errorf("error_code = %q, want %q", code.Code, errLogsClearPasswordWrong)
	}

	// The attempt is written down, and what was typed is not. The line is in
	// the very file the press was about to empty.
	written := logs.FilterMessageSnippet("password that does not open the account").Len()
	if written != 1 {
		t.Errorf("the refusal was written down %d times, want 1", written)
	}

	for _, line := range logs.All() {
		if strings.Contains(line.Message, "not the password") {
			t.Errorf("a log line carries what was typed: %s", line.Message)
		}

		for _, field := range line.Context {
			if strings.Contains(field.String, "not the password") {
				t.Errorf("a log field carries what was typed: %s=%s", field.Key, field.String)
			}
		}
	}
}

// TestEmptyingTheLogWithNoFileConfiguredIsNotFound is the state the read
// answers the same way: there is no file, so there is nothing to empty.
func TestEmptyingTheLogWithNoFileConfiguredIsNotFound(t *testing.T) {
	call := &clearLogsCall{
		db:      clearLogsDB(t),
		body:    `{"password":"` + hostKeyAccountPassword + `"}`,
		account: true,
	}

	rec, _ := call.run(t)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", rec.Code, rec.Body.String())
	}
}

// TestEmptyingTheLogAnswersWhatWentWrong holds a failed emptying to being said
// rather than answered as a press that worked.
func TestEmptyingTheLogAnswersWhatWentWrong(t *testing.T) {
	path := writeLog(t, `{"level":"info","msg":"something happened"}`)

	call := &clearLogsCall{
		path:    path,
		db:      clearLogsDB(t),
		body:    `{"password":"` + hostKeyAccountPassword + `"}`,
		account: true,
		fail:    os.ErrPermission,
	}

	rec, _ := call.run(t)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", rec.Code, rec.Body.String())
	}

	message := decodeRefusal(t, rec)
	if !strings.Contains(message, path) {
		t.Errorf("the refusal reads %q, which does not name the file", message)
	}
}

// TestEmptyingTheLogOutsideTheSessionMiddlewareIsAnError is the route hung
// somewhere the middleware does not cover. It cannot be reached by a request,
// and what it must not be is a press that goes through with no account behind
// it.
func TestEmptyingTheLogOutsideTheSessionMiddlewareIsAnError(t *testing.T) {
	path := writeLog(t, `{"level":"info","msg":"something happened"}`)

	call := &clearLogsCall{
		path: path,
		db:   clearLogsDB(t),
		body: `{"password":"` + hostKeyAccountPassword + `"}`,
	}

	rec, _ := call.run(t)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", rec.Code, rec.Body.String())
	}

	if call.emptied {
		t.Error("the file was emptied with no account on the context")
	}
}
