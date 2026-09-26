package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// The log stays in the file it is written to and is read from there on every
// request, rather than being kept in the database beside everything else this
// API serves. Three things stand in the way of the database:
//
//   - The logger is up before the database is. What it is there to report on
//     first is a database that could not be opened, and a line about that has
//     nowhere to go if the line is a row.
//   - The handle is opened with SetMaxOpenConns(1). Every line would queue on
//     the one connection behind the queries the process is serving.
//   - gorm writes its statements through that same logger. Writing a line would
//     be a statement, which would be written as a line.
//
// So what is here is a reader: the file is the store, and this hands the screen
// the end of it.

// logsDefaultLines is what a request that names no count gets. It is a few
// screens worth of scrolling, which is what an operator opening the screen to
// see what just happened is after.
const logsDefaultLines = 200

// logsMaxLines is the most lines one request can be answered with. A count
// above it is cut down to it rather than refused, so a screen asking for more
// still gets an answer.
//
// The bound exists because the count decides how much of the file is read into
// memory and how large the answer is. A zap line is a few hundred bytes, so
// 2000 of them are well under a megabyte, and 2000 rows are already more than a
// person reads on a screen: past that the file itself is the better tool.
const logsMaxLines = 2000

// logsMaxBytes is how far back from the end of the file a single request may
// read, whatever the line count asks for.
//
// The line count alone does not bound the work: one line has no length limit,
// and a log holding a handful of enormous lines would pull the whole file in
// while counting newlines. The file is allowed to reach 100MB before it rotates
// (settings.LoggingFileMaxSize), so the read needs a bound of its own. 4MB is
// wider than logsMaxLines lines of ordinary length and is a fixed ceiling on
// what one request costs.
const logsMaxBytes = 4 * 1024 * 1024

// logsChunkBytes is how much is read per step while walking backwards through
// the file. The usual request is answered by the first step or two, so the
// common case touches kilobytes of a file that may be a hundred megabytes.
const logsChunkBytes = 64 * 1024

// logsJSONKeys are the fields of a line written by the JSON encoder. They are
// the keys newEncoderConfig leaves at their defaults, except for the timestamp,
// which that function renames.
const (
	logsKeyLevel   = "level"
	logsKeyTime    = "timestamp"
	logsKeyCaller  = "caller"
	logsKeyMessage = "msg"
)

// logLine is one line of the file as the screen draws it.
//
// Raw is always the line as it stands. The fields beside it are filled in when
// the line was recognized, and a line that was not recognized keeps every one
// of them empty and Parsed false rather than being left out of the answer. A
// reader opens this screen because something is wrong, and a line that does not
// look like the others is the one most worth seeing.
type logLine struct {
	Level   string `json:"level"`
	Time    string `json:"time"`
	Caller  string `json:"caller"`
	Message string `json:"message"`
	// Extra is whatever the line carried besides the four fields above: the
	// context fields of a zap line. It is kept as it was written so that no
	// value a line reported is dropped on the way to the screen.
	Extra  string `json:"extra"`
	Raw    string `json:"raw"`
	Parsed bool   `json:"parsed"`
}

// logsAnswer is what the screen is handed.
//
// The path is in it because the screen must not be the side that decides where
// the log is: the server reads the setting against the directory the
// installation lives in, and the screen only reports what was read.
type logsAnswer struct {
	Path string `json:"path"`
	// Lines are oldest first, the order they are in the file. The screen is the
	// side that decides which end it draws first.
	Lines []logLine `json:"lines"`
	// Requested is the count the read was carried out with, after the bound
	// above was applied. A screen that asked for more than logsMaxLines sees
	// here what it was cut down to.
	Requested int `json:"requested"`
	MaxLines  int `json:"max_lines"`
	// Size is the length of the file, and Read is how much of it was touched to
	// answer. The two together are what says the file is not read whole.
	Size int64 `json:"size"`
	Read int64 `json:"read"`
	// Capped is set when logsMaxBytes ended the walk before the asked for
	// number of lines was found, so the screen can say that the oldest line it
	// shows is not as far back as was asked for.
	Capped bool `json:"capped"`
}

// LogsHandler serves the end of the log file.
//
// The path is handed in from the startup rather than read out of the settings
// here, for the same reason the uninstall takes its paths that way: a path
// changed on the Settings screen names a file this process is not writing to
// until it is started again, and the screen has to show the file the lines are
// actually going to.
type LogsHandler struct {
	logger *zap.Logger
	path   string
	// db is here for the one thing this handler does that is not a read: the
	// password of the account is what stands in front of emptying the log.
	db *gorm.DB
	// empty empties the file the logger writes to. It is handed in from the
	// startup rather than done here, because the writer that rotates the log
	// holds the file open across writes and keeps the size it last wrote at.
	// A file emptied from under it would be appended to correctly and rotated
	// far too early, so the emptying has to go through the writer first.
	//
	// It is nil when nothing is writing to a file, which is the same state the
	// empty path above describes.
	empty func() error
}

func NewLogsHandler(logger *zap.Logger, path string, db *gorm.DB, empty func() error) *LogsHandler {
	return &LogsHandler{
		logger: logger,
		path:   path,
		db:     db,
		empty:  empty,
	}
}

// GetLogs answers with the last lines of the log file.
//
// @Summary      The end of the log file
// @Description  The file is read from the end, so read stays small however large the file is.
// @Tags         logs
// @Produce  json
// @Param   lines  query  int  false  "How many lines, up to 2000"
// @Success  200  {object}  models.Response{data=api.logsAnswer}
// @Router       /logs [get]
func (h *LogsHandler) GetLogs(c echo.Context) error {
	count, refused := logsLineCount(c.QueryParam("lines"))
	if refused != nil {
		return refused.answer(c)
	}

	// A path that is empty means the logging settings name no file at all.
	// Answering with an empty list would read as a log with nothing in it,
	// which is a different thing to be told.
	if h.path == "" {
		return failure(c, http.StatusNotFound, errLogsFileNotConfigured)
	}

	raw, tail, err := tailFile(h.path, count)
	if err != nil {
		// A missing file is the shape a startup that could not open the log
		// leaves behind: the process says so on the console and writes there
		// only. The screen cannot see that console, so the reason is spelled
		// out here instead of being answered as an empty log.
		if errors.Is(err, fs.ErrNotExist) {
			return failure(c, http.StatusNotFound, errLogsFileMissing, errorArgs{"path": h.path})
		}

		h.logger.Error("failed to read the log file",
			logid.LoggingFileReadFailed.Field(),
			zap.String("path", h.path),
			zap.Error(err))

		return failure(c, http.StatusInternalServerError, errLogsFileReadFailed, errorArgs{"path": h.path, "reason": err.Error()})
	}

	lines := make([]logLine, 0, len(raw))
	for _, line := range raw {
		lines = append(lines, parseLogLine(line))
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: logsAnswer{
			Path:      h.path,
			Lines:     lines,
			Requested: count,
			MaxLines:  logsMaxLines,
			Size:      tail.size,
			Read:      tail.read,
			Capped:    tail.capped,
		},
	})
}

// logsClearRequest is what the panel sends. The password is the password of the
// account the session belongs to, the same one the uninstall asks for.
type logsClearRequest struct {
	Password string `json:"password"`
}

// ClearLogs empties the log file.
//
// What is emptied is the file this process is writing to and nothing else. The
// rotated files beside it are left as they are: they are what the retention
// settings were set to keep, and a press on the Logs screen is not the place
// those are overruled. The screen reads only the current file, so emptying it
// is what makes the screen empty, which is what the press is for.
//
// The password of the account is asked for. What this does cannot be taken
// back, which is the line the uninstall is on rather than the line the restart
// is on: after a restart the service is running again, and after this the lines
// that were in the file are gone.
//
// @Summary      Empty the file the log is being written to
// @Description  Takes the account password, and leaves the rotated files beside it alone.
// @Tags         logs
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.logsClearRequest  true  "The account password"
// @Success  200  {object}  models.Response{data=api.logsClearedAnswer}
// @Failure  401  {object}  api.errorBody  "The password does not open this account"
// @Router       /logs/clear [post]
func (h *LogsHandler) ClearLogs(c echo.Context) error {
	var req logsClearRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
	}

	// A path that is empty means the logging settings name no file at all, and
	// empty is nil for the same reason. Either one is the state GetLogs answers
	// with the same refusal: there is no file, so there is nothing to empty.
	if h.path == "" || h.empty == nil {
		return failure(c, http.StatusNotFound, errLogsFileNotConfigured)
	}

	// An empty box is refused before the password is checked, so that a press
	// with nothing typed is not answered as a password that is wrong.
	if req.Password == "" {
		return failure(c, http.StatusBadRequest, errLogsClearPasswordMissing)
	}

	refused := accountPasswordRefused(c, h.db, h.logger, req.Password, errLogsClearPasswordWrong)
	if refused != nil {
		// A password that does not open the account is written down. This is a
		// call where a password stands between a session and something that
		// cannot be taken back, so the attempt belongs in the log - which is,
		// this once, the very file the call was about to empty.
		if refused.code == errLogsClearPasswordWrong {
			h.logger.Warn("the log was asked to be emptied with a password that does not open the "+
				"account. The log was not touched",
				logid.LoggingFileClearPasswordWrong.Field())
		}

		return refused.answer(c)
	}

	// The line goes in before the file is emptied, so that it is not the first
	// line of the new file but the last of the old one. What the new file
	// starts with is whatever the service logs next, and a reader who wants to
	// know why the log begins where it does has the answer in the file that was
	// kept if one was kept at all.
	h.logger.Warn("the log file is being emptied from the Logs screen. The rotated files beside it "+
		"are left as they are",
		logid.LoggingFileCleared.Field(),
		zap.String("path", h.path))

	err = h.empty()
	if err != nil {
		h.logger.Error("failed to empty the log file",
			logid.LoggingFileClearFailed.Field(),
			zap.String("path", h.path),
			zap.Error(err))

		return failure(c, http.StatusInternalServerError, errLogsClearFailed,
			errorArgs{"path": h.path, "reason": err.Error()})
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: logsClearedAnswer{
			Path: h.path,
		},
	})
}

// logsClearedAnswer is what the press is answered with. The path is in it for
// the reason it is in the answer of a read: the screen must not be the side
// that decides where the log is, and what it says was emptied has to be what
// the server emptied.
type logsClearedAnswer struct {
	Path string `json:"path"`
}

// logsLineCount reads the lines parameter. An empty one is the default, a count
// above the bound is cut down to it, and anything that is not a positive whole
// number is refused rather than quietly turned into the default: a screen that
// sent a number it worked out wrong has to hear about it.
func logsLineCount(value string) (int, *refusal) {
	if strings.TrimSpace(value) == "" {
		return logsDefaultLines, nil
	}

	count, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, refuse(http.StatusBadRequest, errLogsLinesNotANumber)
	}

	if count < 1 {
		return 0, refuse(http.StatusBadRequest, errLogsLinesBelowOne)
	}

	if count > logsMaxLines {
		return logsMaxLines, nil
	}

	return count, nil
}

// tailStat is what the read touched, so that the answer can report it.
type tailStat struct {
	size   int64
	read   int64
	capped bool
}

// tailFile returns the last count lines of the file at path.
//
// The file is walked backwards a chunk at a time and the walk stops as soon as
// enough line breaks have been passed, so what is read is the end of the file
// and not the file. Reading it whole is what this exists to avoid: the log is
// allowed to reach 100MB before it rotates, and os.ReadFile of that is 100MB
// held in the process for the length of one request, several times over if
// several requests arrive at once.
func tailFile(path string, count int) ([]string, tailStat, error) {
	var stat tailStat

	file, err := os.Open(path)
	if err != nil {
		return nil, stat, err
	}

	defer func() {
		_ = file.Close()
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, stat, err
	}

	stat.size = info.Size()

	// A directory opens and stats without complaint and then reads as garbage,
	// so it is turned away by what it is rather than by what a read of it does.
	if info.IsDir() {
		return nil, stat, errors.New("the path is a directory, not a file")
	}

	offset := stat.size

	// One line break more than the number of lines wanted is what marks where
	// the oldest of them begins. The last one in the file ends the newest line
	// rather than starting a line, so it is counted with them.
	wanted := count + 1

	var (
		chunks [][]byte
		breaks int
	)

	for offset > 0 && breaks < wanted {
		if stat.read >= logsMaxBytes {
			stat.capped = true

			break
		}

		size := int64(logsChunkBytes)
		if room := logsMaxBytes - stat.read; room < size {
			size = room
		}

		if offset < size {
			size = offset
		}

		offset -= size

		chunk := make([]byte, size)

		_, err = file.ReadAt(chunk, offset)
		if err != nil {
			return nil, stat, err
		}

		breaks += bytes.Count(chunk, []byte{'\n'})
		stat.read += size

		// The chunks are collected as they are read, which is back to front,
		// and put in order below. Prepending into one buffer instead would copy
		// everything read so far on every step.
		chunks = append(chunks, chunk)
	}

	for left, right := 0, len(chunks)-1; left < right; left, right = left+1, right-1 {
		chunks[left], chunks[right] = chunks[right], chunks[left]
	}

	data := bytes.Join(chunks, nil)

	// The walk stopped in the middle of the file, so whatever sits before the
	// first line break is the end of a line whose beginning was not read. It is
	// one line further back than was asked for, and the whole of it is in the
	// file, so it is left out.
	//
	// It is kept when the byte limit is what ended the walk. There it is not an
	// extra line but the only one there is room for, and an answer of nothing at
	// all would say the log is empty when it is the opposite. What says the line
	// may be cut is the capped flag that goes with it.
	if offset > 0 && !stat.capped {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			data = data[index+1:]
		} else {
			data = nil
		}
	}

	return splitLogLines(data, count), stat, nil
}

// splitLogLines cuts what was read into at most count lines, oldest first.
//
// A blank line is left out. zap writes none, so one in the file is a leftover
// of something else, and drawing it would be an empty row that says nothing.
func splitLogLines(data []byte, count int) []string {
	lines := make([]string, 0, count)

	for _, line := range strings.Split(string(data), "\n") {
		// A file written on Windows, or one that travelled through a tool that
		// rewrote the endings, carries the carriage return along. It is not
		// part of what was logged and would be drawn as a stray character.
		line = strings.TrimRight(line, "\r")

		if strings.TrimSpace(line) == "" {
			continue
		}

		lines = append(lines, line)
	}

	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}

	return lines
}

// parseLogLine works out what one line says.
//
// The format is read off the line itself and not off the stored logging.format
// setting. The setting is what the process writes in now, while the file holds
// what every process that wrote to it wrote: a format changed on the Settings
// screen leaves both kinds in one file, and trusting the setting would leave
// everything written before the change unreadable.
func parseLogLine(line string) logLine {
	parsed, ok := parseJSONLogLine(line)
	if ok {
		return parsed
	}

	parsed, ok = parseConsoleLogLine(line)
	if ok {
		return parsed
	}

	// Neither shape fits, so the line is handed over as it stands. It is not
	// dropped: a line that does not parse is either something else writing into
	// this file or a line that was cut short, and both are worth seeing.
	return logLine{Raw: line}
}

// parseJSONLogLine reads a line written by the JSON encoder.
func parseJSONLogLine(line string) (logLine, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "{") {
		return logLine{}, false
	}

	// The fields are decoded as raw values rather than into a struct, so that
	// the ones this does not name can be handed on untouched.
	var fields map[string]json.RawMessage

	err := json.Unmarshal([]byte(trimmed), &fields)
	if err != nil {
		return logLine{}, false
	}

	// A JSON object that carries neither a level nor a message is not one of
	// these lines. Reading it as one would leave a row that is blank where the
	// eye looks first.
	_, hasLevel := fields[logsKeyLevel]
	_, hasMessage := fields[logsKeyMessage]

	if !hasLevel && !hasMessage {
		return logLine{}, false
	}

	parsed := logLine{
		Level:   strings.ToLower(logsJSONString(fields, logsKeyLevel)),
		Time:    logsJSONString(fields, logsKeyTime),
		Caller:  logsJSONString(fields, logsKeyCaller),
		Message: logsJSONString(fields, logsKeyMessage),
		Raw:     line,
		Parsed:  true,
	}

	for _, key := range []string{logsKeyLevel, logsKeyTime, logsKeyCaller, logsKeyMessage} {
		delete(fields, key)
	}

	if len(fields) > 0 {
		// json.Marshal writes the keys of a map in order, so the same line
		// always comes out the same way and the screen does not reshuffle the
		// fields between two refreshes.
		extra, err := json.Marshal(fields)
		if err == nil {
			parsed.Extra = string(extra)
		}
	}

	return parsed, true
}

// logsJSONString reads one field as text. A field that was written as something
// other than a string is handed on as it was written rather than being dropped,
// which is what a logger configured elsewhere might leave in the level field.
func logsJSONString(fields map[string]json.RawMessage, key string) string {
	value, ok := fields[key]
	if !ok {
		return ""
	}

	var text string

	err := json.Unmarshal(value, &text)
	if err != nil {
		return strings.TrimSpace(string(value))
	}

	return text
}

// logsConsoleLevels are the levels zap writes, used to tell a console line from
// a line of something else. They are matched without regard to case because the
// encoder can be configured either way, and the level of a line that is read
// here is lowered so that one vocabulary reaches the screen.
var logsConsoleLevels = map[string]bool{
	"debug":  true,
	"info":   true,
	"warn":   true,
	"error":  true,
	"dpanic": true,
	"panic":  true,
	"fatal":  true,
}

// parseConsoleLogLine reads a line written by the console encoder. That encoder
// separates the fields with tabs and writes them in a fixed order: the time,
// the level, the caller and the message, followed by whatever context the line
// carried.
func parseConsoleLogLine(line string) (logLine, bool) {
	parts := strings.Split(line, "\t")
	if len(parts) < 3 {
		return logLine{}, false
	}

	level := strings.ToLower(strings.TrimSpace(parts[1]))
	if !logsConsoleLevels[level] {
		return logLine{}, false
	}

	parsed := logLine{
		Level:  level,
		Time:   strings.TrimSpace(parts[0]),
		Raw:    line,
		Parsed: true,
	}

	// The caller is there when the logger was built with zap.AddCaller, and
	// this process builds it that way. A line from one that was not still has
	// to read, so the field is taken as the caller only when it looks like one.
	rest := parts[2:]
	if len(rest) > 1 && logsLooksLikeCaller(rest[0]) {
		parsed.Caller = rest[0]
		rest = rest[1:]
	}

	parsed.Message = rest[0]

	if len(rest) > 1 {
		parsed.Extra = strings.Join(rest[1:], "\t")
	}

	return parsed, true
}

// logsLooksLikeCaller reports whether a field is a "file.go:123" the caller
// field is written as. A message happens to hold a colon often enough that the
// digits after it are what decides.
func logsLooksLikeCaller(value string) bool {
	index := strings.LastIndex(value, ":")
	if index <= 0 || index == len(value)-1 {
		return false
	}

	if strings.ContainsAny(value, " \t") {
		return false
	}

	for _, digit := range value[index+1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}

	return true
}
