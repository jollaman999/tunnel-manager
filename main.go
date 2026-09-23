package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gorm.io/gorm"

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/database"
	"github.com/jollaman999/tunnel-manager/internal/install"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/tlsserve"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/jollaman999/tunnel-manager/internal/web"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const version = "3.9.0"

// Maximum time to wait for in-flight HTTP requests to finish on shutdown.
const shutdownTimeout = 10 * time.Second

// Maximum time to wait for the reconcile loop to return on shutdown. The loop
// leaves as soon as the pass it is running ends, and a pass that is dialing a
// Host that does not answer takes as long as the SSH timeouts of the tunnels it
// still has to start. Waiting for it without a bound would hold the process up
// long after the API stopped answering.
const reconcileStopTimeout = 10 * time.Second

// bootstrapLogLevel is the level the startup logs at until the stored settings
// say otherwise. The level is one of those settings, and it is read out of the
// database, so the steps that open the database have to log at some level
// chosen without it. "info" is the level a fresh deployment gets anyway.
const bootstrapLogLevel = "info"

// apiPrefix is the path every route that is not the UI hangs under, and
// importTunnelsPath and importSettingsPath are the two that take a whole
// exported file in the body.
//
// The three are named rather than written out at the call because the body
// limit below is decided by the route: generalBodyLimit skips these two and
// they carry importBodyLimit instead. A path typed twice would drift, and the
// half that drifted would be the skipper, which fails as an import that is
// refused at a size that used to work.
const (
	apiPrefix          = "/api"
	importTunnelsPath  = "/import/tunnels"
	importSettingsPath = "/import/settings"
)

// generalBodyLimit is the most a request body may hold on every route but the
// two imports. Without it a body is read into memory until the client stops
// sending, and POST /api/login is reached before any session exists, so
// anybody who can open the port decides how much this process allocates.
//
// A megabyte is far more than any of these routes needs. The largest is the
// registration of a TLS certificate (PUT /api/certificate), which carries a
// certificate chain and a PEM private key, and the creation of a Host, which
// carries one PEM private key: a 4096-bit RSA key is around 3KB and a chain a
// few KB more.
const generalBodyLimit = "1M"

// importBodyLimit is what the two imports are allowed instead. Their body is a
// whole exported configuration, and the size of that follows from how much the
// installation that wrote it held rather than from anything this code decides.
//
// One Host in an exported file carries its PEM private key, its passphrase and
// its SSH password in the clear (internal/api/transfer.go, hostContent), which
// is a few KB, and the whole file is then base64 encoded, which adds a third.
// So a Host costs on the order of 5KB of body and a service port a hundred
// bytes, and the local port being unique caps the service ports at 65535.
// 32MB carries a few thousand Hosts or every service port an installation can
// hold. An import is behind the session, so what this allows is an
// administrator who is already authenticated.
const importBodyLimit = "32M"

// The timeouts the API server runs under. echo builds its http.Server with all
// of them at zero, which means a client that opens a connection and says
// nothing holds it until the process ends.
const (
	// apiReadHeaderTimeout bounds how long the request head may take. It is
	// the same ten seconds the plaintext redirect server on the same port
	// already uses (internal/tlsserve, peekTimeout): a client writes its
	// request straight after connecting, so this covers the network in
	// between and not any thinking on its part.
	apiReadHeaderTimeout = 10 * time.Second
	// apiReadTimeout bounds the head and the body together. It has to hold
	// the largest import importBodyLimit allows, so it is minutes rather than
	// seconds: 32MB over a link of a couple of megabits is already past a
	// minute, and a body cut off half way is an import that failed for a
	// reason nothing in the answer explains. The head is bounded far more
	// tightly by the deadline above, which is what a client that dribbles a
	// request head is stopped by.
	apiReadTimeout = 5 * time.Minute
	// apiIdleTimeout is how long a kept-alive connection may sit between
	// requests. The screens make a handful of calls per draw and poll the
	// status, so two minutes keeps the connection they are using while
	// clearing up the ones nobody came back to.
	apiIdleTimeout = 2 * time.Minute
)

// No WriteTimeout is set, on purpose.
//
// net/http arms the write deadline when the request head has been read and not
// when the handler starts answering (net/http.conn.readRequest, the deferred
// SetWriteDeadline). One value therefore has to cover reading the body,
// running the handler and writing the answer. To leave the largest import room
// it would have to be longer than apiReadTimeout, which makes it no bound on
// the answer at all, and any value short enough to be one would cut those
// imports off.
//
// What it would otherwise protect against is a client that reads the answer a
// byte at a time. The answers here are bounded - the log screen reads at most
// 4MB of the file per request (internal/api, logsMaxBytes) and everything else
// is smaller - so such a client holds one connection and no unbounded memory,
// and apiIdleTimeout takes the connection once the answer is out.

// hstsMaxAgeSeconds is how long a browser is told to reach this installation
// over HTTPS only. It is sent solely while a certificate somebody else signed
// is being served: see securityHeaders.
//
// Thirty days rather than the year that is usual elsewhere. The certificate
// here can be put back to a self-signed one from the Settings screen at any
// time, and a browser holding this header refuses to let anybody click through
// the warning that follows, with no way back but clearing it by hand in the
// browser. A month is long enough to cover the life of a session and short
// enough that a mistake ends.
const hstsMaxAgeSeconds = 30 * 24 * 60 * 60

// coreSwitch holds the core a logger writes through so that it can be exchanged
// while the logger stays the same object.
//
// The startup needs a logger before it has any setting, because the settings
// are in the database and opening it is the step most likely to fail. Building
// a second logger once they are read would leave everything that was handed the
// first one writing to the console for the life of the process, gorm among
// them, and gorm is what reports a query that fails. So the core behind the one
// logger is replaced instead.
type coreSwitch struct {
	current atomic.Pointer[zapcore.Core]
}

func newCoreSwitch(core zapcore.Core) *coreSwitch {
	s := &coreSwitch{}
	s.set(core)

	return s
}

func (s *coreSwitch) set(core zapcore.Core) {
	s.current.Store(&core)
}

func (s *coreSwitch) load() zapcore.Core {
	return *s.current.Load()
}

// switchedCore is the core of a logger built on a coreSwitch. Every call reads
// the core that is in the switch at that moment, so the exchange reaches the
// loggers that were handed out before it.
type switchedCore struct {
	swap   *coreSwitch
	fields []zapcore.Field
}

func (c *switchedCore) Enabled(level zapcore.Level) bool {
	return c.swap.load().Enabled(level)
}

// With keeps the fields beside the switch instead of folding them into the core
// they were added to, since that core is replaced later on and a child logger
// built before the exchange has to write through the new one as well.
func (c *switchedCore) With(fields []zapcore.Field) zapcore.Core {
	joined := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	joined = append(joined, c.fields...)
	joined = append(joined, fields...)

	return &switchedCore{swap: c.swap, fields: joined}
}

func (c *switchedCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}

	return checked
}

func (c *switchedCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	core := c.swap.load()
	if len(c.fields) > 0 {
		core = core.With(c.fields)
	}

	return core.Write(entry, fields)
}

func (c *switchedCore) Sync() error {
	return c.swap.load().Sync()
}

func newEncoderConfig() zapcore.EncoderConfig {
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	return encoderConfig
}

// newBootstrapLogger returns the logger the startup runs on until the settings
// are read, together with the switch that replaces its core once they are. It
// writes to the console and nowhere else: the log file is a setting, and the
// step that would say where that file is is the very one this logger is there
// to report on.
func newBootstrapLogger() (*zap.Logger, *coreSwitch) {
	var level zapcore.Level

	// bootstrapLogLevel is a constant of this file, so it parses.
	_ = level.UnmarshalText([]byte(bootstrapLogLevel))

	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(newEncoderConfig()),
		zapcore.AddSync(os.Stdout),
		level,
	)

	swap := newCoreSwitch(core)

	return zap.New(&switchedCore{swap: swap}, zap.AddCaller()), swap
}

// resolveInstallPath reads a stored path against the directory the database
// file is in rather than against the working directory.
//
// The working directory is not the same twice: systemd leaves it at / unless a
// unit says otherwise, the container image sets it to /, and a person running
// the binary is wherever they happened to be. A relative default read against
// it puts the key and the log somewhere different every time, which is how an
// earlier release wrote the encryption key into the root of the filesystem.
//
// The directory holding the database is what this installation is, so the key,
// the log and the initial password all sit beside it. An absolute path is left
// alone and wins.
func resolveInstallPath(installDir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(installDir, path)
}

// logDirMode and logFileMode are what the log file and the directory holding
// it are left at.
//
// The log is not a file of secrets, but it is read back through GET /api/logs
// behind the session check, and the lines it carries say which hosts this
// installation reaches, under which account names, and what a failed query was.
// The account password is not in it, yet a file the rest of the machine can
// read hands a local user the map of everything this installation manages, so
// it is kept to the account this process runs as, as the database and the
// encryption key beside it are.
const (
	logDirMode  os.FileMode = 0700
	logFileMode os.FileMode = 0600
)

// prepareLogFile makes sure the configured log file can be written to.
func prepareLogFile(s *settings.Settings, installDir string) error {
	logFilePath := resolveInstallPath(installDir, s.LoggingFilePath)
	logDir := filepath.Dir(logFilePath)
	err := os.MkdirAll(logDir, logDirMode)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}

	// The directory is narrowed as well as created, because MkdirAll writes the
	// mode only for the directories it makes and an installation from an
	// earlier release has one that is already there at 0755.
	//
	// What this and the call below report is dropped, and that is the whole of
	// the handling. A mode that cannot be set is no reason to stop writing logs:
	// the file opened, and an installation on a file system that carries no
	// Unix modes would otherwise come up with its logging turned off over a
	// file it has no way of narrowing.
	_ = os.Chmod(logDir, logDirMode)

	file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFileMode)
	if err != nil {
		return fmt.Errorf("failed to create log file: %v", err)
	}

	// The mode handed to OpenFile is masked by the umask and is written only
	// for a file that is being created, so it is set again on the descriptor
	// that was opened. That covers the log an earlier release left at 0644,
	// and it is done through the descriptor rather than the path so that it
	// reaches the file that was just opened and not whatever appeared under
	// that name in the meantime.
	//
	// The rotated files follow from this one. lumberjack copies the mode of the
	// file it rotates onto the one it opens next and onto the compressed copy
	// it writes, so a current log at 0600 is a whole directory of them at 0600.
	_ = file.Chmod(logFileMode)

	// The file is opened to see that it can be written to, nothing else. The
	// logs go through lumberjack, which opens the file on its own, so holding
	// this one open would keep a descriptor for the life of the process.
	err = file.Close()
	if err != nil {
		return fmt.Errorf("failed to close log file: %v", err)
	}

	return nil
}

// initLogger builds the core the process logs through from the stored settings,
// and returns the level handle along with it. The level is held in an
// AtomicLevel rather than fixed into the core so that a change made on the
// Settings screen takes hold without a restart, which is what the screen says
// about it.
//
// A core is returned instead of a logger because the logger already exists by
// the time this is called: the startup built one to report on opening the
// database, and this core is put into it.
//
// The third return is what empties the log file, which the Logs screen offers
// as a press. It comes from here because the writer that holds the file open
// is built here and the emptying has to go through it: see emptyLogFile. It is
// nil when nothing is writing to a file.
func initLogger(s *settings.Settings, installDir string) (zapcore.Core, zap.AtomicLevel,
	func() error, error) {
	// An unusable log file is not fatal. The logger falls back to the console
	// only, but the reason has to be visible since nothing is written to the file.
	fileErr := prepareLogFile(s, installDir)
	if fileErr != nil {
		log.Printf("Logging to file is disabled: %v (path: %s)", fileErr,
			resolveInstallPath(installDir, s.LoggingFilePath))
	}

	level := zap.NewAtomicLevel()
	err := level.UnmarshalText([]byte(s.LoggingLevel))
	if err != nil {
		return nil, level, nil, fmt.Errorf("failed to parse log level: %v", err)
	}

	encoderConfig := newEncoderConfig()

	var encoder zapcore.Encoder
	if s.LoggingFormat == "json" {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	var (
		cores []zapcore.Core
		empty func() error
	)

	if fileErr == nil {
		path := resolveInstallPath(installDir, s.LoggingFilePath)

		logWriter := &lumberjack.Logger{
			Filename:   path,
			MaxSize:    s.LoggingFileMaxSize,
			MaxBackups: s.LoggingFileMaxBackups,
			MaxAge:     s.LoggingFileMaxAge,
			Compress:   s.LoggingFileCompress,
		}

		empty = func() error {
			return emptyLogFile(logWriter, path)
		}

		cores = append(cores, zapcore.NewCore(
			encoder,
			zapcore.AddSync(logWriter),
			level,
		))
	}

	cores = append(cores, zapcore.NewCore(
		encoder,
		zapcore.AddSync(os.Stdout),
		level,
	))

	core := zapcore.NewTee(cores...)

	if fileErr != nil {
		zap.New(core).Warn("logging to file is disabled",
			logid.LoggingToFileDisabled.Field(),
			zap.String("path", s.LoggingFilePath),
			zap.Error(fileErr),
			zap.String("message", "logs are written to the console only"))
	}

	return core, level, empty, nil
}

// emptyLogFile empties the file the rotating writer is writing to.
//
// The writer is closed before the file is cut. It holds the file open across
// writes and carries the size it last wrote at, and neither is read again
// while the handle is open: cutting the file under it would leave that size
// far larger than the file, and the next line of any length would be taken for
// one that fills the file and rotate it on the spot. Closed, the writer opens
// the file again on the next line and reads its size then, which is nought.
//
// The file is cut rather than removed so that it keeps the mode and the owner
// it was given. Removed, it would be created again by the writer with the mode
// that writer defaults to, which is not the one the startup narrowed it to.
//
// A line written in the moment between the close and the cut is lost. It is a
// line from before the press that asked for the file to be emptied, which is
// what the press is throwing away.
func emptyLogFile(writer *lumberjack.Logger, path string) error {
	err := writer.Close()
	if err != nil {
		return fmt.Errorf("failed to close the log file: %v", err)
	}

	err = os.Truncate(path, 0)
	if err != nil {
		return fmt.Errorf("failed to empty the log file: %v", err)
	}

	return nil
}

// storedPasswordCheck holds what the stored passwords answered when they were
// read with the encryption key in use.
type storedPasswordCheck struct {
	decrypted          int
	notEncrypted       int
	markedNotOpened    int
	hostsThatDoNotOpen []uint
}

// checkStoredPasswords sorts the stored password of every Host into one that
// opens with the key in use, one that was stored before passwords were
// encrypted, and one that does not open. A value that does not open is counted
// separately when it carries the marker that Encrypt writes, because only then
// is it certain that it was encrypted at all.
func checkStoredPasswords(hosts []models.Host, cipher *crypto.Cipher) storedPasswordCheck {
	var check storedPasswordCheck

	for _, host := range hosts {
		_, err := cipher.Decrypt(host.Password)
		switch {
		case err == nil:
			check.decrypted++
		case errors.Is(err, crypto.ErrNotEncrypted):
			check.notEncrypted++
		default:
			check.hostsThatDoNotOpen = append(check.hostsThatDoNotOpen, host.ID)
			if crypto.IsEncrypted(host.Password) {
				check.markedNotOpened++
			}
		}
	}

	return check
}

// keyIsWrong reports whether the key in use is not the one the stored passwords
// were sealed with. A process holds a single key, so a wrong one fails every
// Host at once, and one password that opens is proof that the key is the right
// one.
//
// Only a value carrying the marker proves the opposite. The marker holds a ':',
// which the base64 alphabet does not, so a marked value can be nothing but the
// output of Encrypt. A value that does not open and carries no marker is either
// the encrypted format from before the marker existed or a password stored
// before encryption that happens to be spelled in base64 characters, and a
// deployment upgraded from that time holds nothing but such passwords. Refusing
// to start over one of them would keep the API that sets a password again from
// ever coming up.
func (c storedPasswordCheck) keyIsWrong() bool {
	return c.markedNotOpened > 0 && c.decrypted == 0
}

// checkEncryptionKey stops the startup when the configured key file opens none
// of the stored passwords. Every tunnel needs a password that opens, so keeping
// the process up with the wrong key would serve an API that looks healthy while
// no Host can be connected to.
func checkEncryptionKey(db *gorm.DB, cipher *crypto.Cipher, logger *zap.Logger, keyFile string) {
	var hosts []models.Host

	err := db.Find(&hosts).Error
	if err != nil {
		// A read that fails says nothing about the key, and restoring the
		// tunnels reports the same failure, so the startup goes on.
		logger.Warn("failed to read the Hosts to check the encryption key against",
			logid.EncryptionKeyCheckHostsReadFailed.Field(),
			zap.Error(err))
		return
	}

	check := checkStoredPasswords(hosts, cipher)

	if check.keyIsWrong() {
		logger.Fatal("the configured key file opens none of the stored passwords, so it is not the key "+
			"they were encrypted with. Put the key file that the passwords were stored with back in place, "+
			"or set the password of every Host again through the API. Starting against another key is "+
			"refused because no tunnel could be built and no stored password could be read",
			logid.EncryptionKeyOpensNoStoredPassword.Field(),
			zap.String("key_file", keyFile),
			zap.Int("hosts", len(hosts)),
			zap.Int("hosts_that_do_not_open", len(check.hostsThatDoNotOpen)),
			zap.Uints("host_ids_that_do_not_open", check.hostsThatDoNotOpen))
	}

	// Whatever the reason a password does not open, the operator does the same
	// thing about it, so it is one message and not one per reason.
	if len(check.hostsThatDoNotOpen) > 0 {
		logger.Warn("the stored password of these Hosts does not open with the encryption key in use, so no "+
			"tunnel is built for them. Set their password again through the API. The stored value is left as "+
			"it is, because a password that does not open exists nowhere else and is gone once it is written over",
			logid.EncryptionStoredPasswordDoesNotOpen.Field(),
			zap.String("key_file", keyFile),
			zap.Int("hosts_that_do_not_open", len(check.hostsThatDoNotOpen)),
			zap.Uints("host_ids_that_do_not_open", check.hostsThatDoNotOpen),
			zap.Int("hosts_that_open", check.decrypted))
	}

	switch {
	case check.decrypted > 0:
		logger.Info("the encryption key opens the stored passwords",
			logid.EncryptionKeyOpensStoredPasswords.Field(),
			zap.String("key_file", keyFile),
			zap.Int("hosts_that_open", check.decrypted))
	case len(check.hostsThatDoNotOpen) == 0:
		logger.Info("no stored password is encrypted, so there is nothing to check the encryption key against",
			logid.EncryptionNoStoredPasswordEncrypted.Field(),
			zap.Int("hosts", len(hosts)))
	}
}

// ensureUser creates the single account on the first startup. The initial
// password is written to a file next to the database file and never to the
// log, which goes to the console as well as to the log file, so only the path is
// reported. The encryption key file is handled the same way.
func ensureUser(db *gorm.DB, logger *zap.Logger, passwordFile string) {
	created, err := auth.EnsureUser(db, passwordFile)
	if err != nil {
		// The startup stops here rather than going on with a warning. Nothing
		// can log in while the account is missing, so an API that came up would
		// answer nobody, and a directory that cannot be written to is a
		// deployment question the operator has to settle once: point -db at a
		// directory the process may write to, or give it that permission.
		logger.Fatal("failed to set up the account. The initial password is written next to the "+
			"database file, so the process has to be allowed to write to that directory",
			logid.AccountInitialSetupFailed.Field(),
			zap.Error(err),
			zap.String("initial_password_file", passwordFile))
	}

	if created {
		logger.Info("created the account with an initial password. Read the password from the file, log in "+
			"with it, and set a username and a password. The file is written with permission 0600 and holds "+
			"the only copy of the password",
			logid.AccountCreatedWithInitialPassword.Field(),
			zap.String("initial_password_file", passwordFile))
		return
	}

	logger.Info("the account is already set up", logid.AccountAlreadySetUp.Field())
}

type CustomValidator struct {
	validator *validator.Validate
}

// proxyTrustSetter is the half of the authentication handler this file uses to
// pass the -trust-proxy-headers flag on. It is an interface of one method so
// that what the flag does can be held to in a test: the handler keeps the
// answer to itself, and a test that was handed the handler could only look at
// the cookies of a request to find out.
type proxyTrustSetter interface {
	TrustProxyHeaders(trust bool)
}

// applyProxyTrust turns the flag on where it is on and says nothing where it
// is not. It calls nothing for the flag that was left out rather than passing
// false, so that off is the state the handler was built in: what is in front
// of this server is believed only where the operator said something is.
func applyProxyTrust(setter proxyTrustSetter, trust bool) {
	if !trust {
		return
	}

	setter.TrustProxyHeaders(true)
}

// indexHTML is the page the UI is drawn from. It is embedded here as well as
// in internal/web, which serves it, because the Content-Security-Policy below
// has to name the scripts written inside that page and there is no way to name
// an inline script but by the hash of its text.
//
// Reading the file rather than keeping the hashes as constants is what makes
// the policy follow the page: somebody editing the theme or the language block
// at the top of index.html changes what its hash is, and a policy holding the
// old one leaves a page that comes up blank with an error only the browser
// console shows.
//
//go:embed internal/web/static/index.html
var indexHTML []byte

// inlineScriptHashes is the CSP source list entry for every script written
// inside html, in the order they appear. A script that names a file with src
// is left out: it is fetched from this origin and 'self' already covers it.
//
// The hash is over the text between the tags exactly as it stands, whitespace
// and all, because that is what a browser hashes when it checks the policy.
func inlineScriptHashes(html []byte) []string {
	var hashes []string

	rest := html

	for {
		open := bytes.Index(rest, []byte("<script"))
		if open < 0 {
			break
		}

		rest = rest[open+len("<script"):]

		tagEnd := bytes.IndexByte(rest, '>')
		if tagEnd < 0 {
			break
		}

		attributes := rest[:tagEnd]
		body := rest[tagEnd+1:]

		end := bytes.Index(body, []byte("</script>"))
		if end < 0 {
			break
		}

		text := body[:end]
		rest = body[end+len("</script>"):]

		if bytes.Contains(attributes, []byte("src")) {
			continue
		}

		sum := sha256.Sum256(text)
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}

	return hashes
}

// contentSecurityPolicy is what every answer carries. It is built from the
// page rather than written out, because of the inline scripts: see indexHTML.
//
// The default is 'none' and every kind of load the UI actually makes is listed
// from there, so a kind nobody thought of is refused rather than allowed. What
// the UI loads was read off the files it is made of: style.css is the one
// stylesheet and holds no url() and no @import, screens.js and app.js are
// fetched from this origin, the catalogs and /api/** are fetched with
// window.fetch from this origin, and there is no image, font, frame, worker or
// plugin anywhere in them.
//
// img-src is listed even though no page names an image, because a browser asks
// for /favicon.ico on its own and a refused one is an error in the console of
// every operator who opens the screens.
//
// frame-ancestors says what X-Frame-Options says. Both are sent: the header is
// what an older browser reads and the directive is what a current one reads,
// and a current one ignores the header where the directive is present.
func contentSecurityPolicy(html []byte) string {
	scripts := append([]string{"'self'"}, inlineScriptHashes(html)...)

	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self'",
		"img-src 'self'",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// apiDocsContentSecurityPolicy is what the documentation page carries instead.
// It is built here beside the policy above so that the two are read together,
// and it is put on by the route that serves that page and by nothing else: the
// screens of this product keep the policy above, and what is widened below is
// widened for /ui/api-docs/ alone.
//
// Swagger UI is somebody else's code and does not come up under the policy the
// screens run under. Each difference is a thing it does, read off the files it
// is made of and confirmed by opening the page in a browser with the console
// watched.
//
//   - script-src is 'self' and nothing else. The page names its two scripts as
//     files rather than writing either of them inside itself, so there is no
//     hash to list and no reason for 'unsafe-inline' to appear here, which is
//     the one entry that would let anything at all run on this origin.
//   - style-src has to carry 'unsafe-inline'. The bundle writes style elements
//     into the document while it runs, and there is no hash that covers what a
//     script writes after the page has loaded, so a policy that omits this
//     leaves the documentation unreadable rather than unsafe.
//   - img-src has to carry data:, because the icons of Swagger UI are written
//     into swagger-ui.css as data URLs rather than fetched as files.
//   - connect-src is 'self' and no more. That is what "Try it out" calls, and
//     it reaches this server and nowhere else.
//
// Nothing in the list reaches off this origin. That is the point of serving
// the files out of the binary, and the test over this policy holds it to it.
const apiDocsContentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"base-uri 'none'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'"

// caIssuedCertificate reports whether the certificate being served was signed
// by somebody other than itself, which is to say that the operator registered
// one rather than letting this installation make its own.
//
// The subject and the issuer are compared as they were encoded, which is what
// a client building a chain compares, and it is the same test the Settings
// screen shows a certificate as self-signed by (internal/api, viewOf).
//
// Everything it cannot answer is answered with false. This decides whether a
// browser is told to refuse plaintext for the next month, and the one thing
// that must not happen is that being turned on for an installation serving a
// certificate no client trusts.
func caIssuedCertificate(keyPair *tls.Certificate) bool {
	if keyPair == nil {
		return false
	}

	leaf := keyPair.Leaf

	if leaf == nil {
		if len(keyPair.Certificate) == 0 {
			return false
		}

		parsed, err := x509.ParseCertificate(keyPair.Certificate[0])
		if err != nil {
			return false
		}

		leaf = parsed
	}

	return !bytes.Equal(leaf.RawSubject, leaf.RawIssuer)
}

// securityHeaders puts the headers on every answer this server writes, the API
// and the UI alike. servedCertificate is read per request rather than once,
// because the certificate is replaced while the process runs: an operator who
// registers a real one on the Settings screen gets the header from the next
// request, and one who goes back to a self-signed one stops getting it.
//
// Strict-Transport-Security is the only one that is conditional, and it is
// held to two things at once. The request has to have arrived over TLS, and
// the certificate being served has to be one somebody else signed. A
// self-signed certificate is what a fresh installation serves, and a browser
// that was told to stay on HTTPS refuses to let anybody past the warning such
// a certificate raises, with no way back from the screens themselves. The
// header is not sent over plaintext either way, which is what a client would
// ignore, so nothing is said where nothing can be meant.
func securityHeaders(policy string, servedCertificate func() *tls.Certificate) echo.MiddlewareFunc {
	hsts := fmt.Sprintf("max-age=%d", hstsMaxAgeSeconds)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			header := c.Response().Header()

			header.Set(echo.HeaderXFrameOptions, "DENY")
			header.Set(echo.HeaderXContentTypeOptions, "nosniff")
			header.Set(echo.HeaderContentSecurityPolicy, policy)

			if c.IsTLS() && caIssuedCertificate(servedCertificate()) {
				header.Set(echo.HeaderStrictTransportSecurity, hsts)
			}

			return next(c)
		}
	}
}

// isImportRoute is the one place that says which routes carry a body of their
// own size. It reads the registered path and not the one that was asked for,
// so a request that spells the path some other way is measured against the
// general limit.
func isImportRoute(c echo.Context) bool {
	switch c.Path() {
	case apiPrefix + importTunnelsPath, apiPrefix + importSettingsPath:
		return true
	}

	return false
}

// importBodyLimitMiddleware is what the two import routes are registered with.
// It is a function rather than one value shared by both, because the
// middleware keeps a pool of readers and there is no reason for the two routes
// to contend on one.
func importBodyLimitMiddleware() echo.MiddlewareFunc {
	return middleware.BodyLimit(importBodyLimit)
}

// applyServerTimeouts puts the deadlines on the servers echo would otherwise
// run with none. Both are set: e.Server is what serves while HTTPS is off and
// e.TLSServer is what serves while it is on, and which of them main starts is
// decided several hundred lines later.
func applyServerTimeouts(servers ...*http.Server) {
	for _, server := range servers {
		server.ReadHeaderTimeout = apiReadHeaderTimeout
		server.ReadTimeout = apiReadTimeout
		server.IdleTimeout = apiIdleTimeout
	}
}

// harden is everything this file does to the HTTP server that is not a route:
// the body limits, the headers and the deadlines. It is one function so that a
// test serves through the same wiring main does rather than through a copy of
// it that has drifted.
//
// The headers go on before the body limit and not after it. Everything put on
// the response before the handler runs is carried by whatever answer leaves,
// the ones no handler wrote included: the 413 the limit below refuses an
// oversized body with, and the 304 the UI answers a fresh browser cache with,
// both go out through the same response and both carry the headers.
func harden(e *echo.Echo, servedCertificate func() *tls.Certificate) {
	e.Use(securityHeaders(contentSecurityPolicy(indexHTML), servedCertificate))
	e.Use(middleware.BodyLimitWithConfig(middleware.BodyLimitConfig{
		Skipper: isImportRoute,
		Limit:   generalBodyLimit,
	}))

	applyServerTimeouts(e.Server, e.TLSServer)
}

func (cv *CustomValidator) Validate(i interface{}) error {
	cv.validator.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name, _, _ := strings.Cut(fld.Tag.Get("json"), ",")
		if name == "-" || name == "" {
			return fld.Name
		}
		return name
	})
	return cv.validator.Struct(i)
}

// repairStoredPaths puts a stored log file or encryption key file that names a
// place outside the installation directory back to its default, and says in
// the log what it replaced. It runs before the settings are read, because the
// read is what would otherwise refuse such a row.
//
// The line is a warning and not an error: the process goes on, and it goes on
// with the path the rule allows rather than the one that was stored. What is
// gone is what the operator wrote, so the line carries both halves of it.
//
// A write that fails is reported and left there. The read that follows holds
// the row against the same rules and stops the startup naming -reset-settings,
// which is the advice this could only repeat.
func repairStoredPaths(db *gorm.DB, logger *zap.Logger) {
	changes, err := settings.RepairPaths(db)
	if err != nil {
		logger.Warn("failed to put a stored path setting back to its default",
			logid.SettingsStoreFailed.Field(),
			zap.Error(err))

		return
	}

	for _, change := range changes {
		logger.Warn("a stored path setting names a place outside the directory the database file "+
			"is in, which is no longer allowed, and was put back to its default",
			logid.SettingsSettingPutBackToDefault.Field(),
			zap.String("setting", change.Name),
			zap.String("from", change.From),
			zap.String("to", change.To))
	}
}

// resetStoredSettings puts every stored setting back to its default and ends
// the process. It is the way out of a stored set that keeps the process from
// starting: the settings are changed on a screen this binary serves, and a
// setting that stops the startup leaves no screen to change it on. It exits
// rather than going on, because the process read nothing yet and going on would
// serve settings that were replaced a moment ago.
func resetStoredSettings(db *gorm.DB, logger *zap.Logger) {
	before, after, err := settings.Reset(db)
	if err != nil {
		logger.Fatal("failed to put the settings back to their defaults", logid.SettingsResetFailed.Field(), zap.Error(err))
	}

	changes := settings.Diff(before, after)

	if len(changes) == 0 {
		logger.Info("every setting was already at its default, so nothing was changed",
			logid.SettingsAlreadyAtDefault.Field())
	}

	for _, change := range changes {
		if before == nil {
			logger.Info("stored a setting that the database did not hold yet",
				logid.SettingsMissingSettingStored.Field(),
				zap.String("setting", change.Name),
				zap.String("to", change.To))
			continue
		}

		logger.Info("put a setting back to its default",
			logid.SettingsSettingPutBackToDefault.Field(),
			zap.String("setting", change.Name),
			zap.String("from", change.From),
			zap.String("to", change.To))
	}

	// os.Exit runs no deferred call, so what was logged is flushed here.
	_ = logger.Sync()
	os.Exit(0)
}

// databaseFileName is what the database file is called under the directory the
// platform keeps user data in.
const databaseFileName = "tunnel-manager.db"

// defaultDatabasePath returns the file to use when -db does not name one. It is
// worked out from os.UserConfigDir rather than from the working directory,
// which differs between running from the repository, from the container and
// from systemd, and would put a fresh empty database wherever the process
// happened to be started.
//
// A missing HOME is reported rather than worked around. Inventing a location
// would let one startup build a database in one place and the next one build
// another somewhere else, and the Hosts that were registered would look gone.
func defaultDatabasePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no default location for the database file is available: %w. "+
			"Give -db an absolute path", err)
	}

	return filepath.Join(dir, "tunnel-manager", databaseFileName), nil
}

// installation is what the four installation flags were given, gathered so that
// the checks between them are made in one place.
//
// binNamed and databaseNamed are carried beside the values because "left out"
// and "given" are different answers here and an empty string cannot tell them
// apart. -db is filled in from defaultDatabasePath for a server that is
// starting, and an install that took that value would register the service with
// the database of whoever happened to run the install instead of the one the
// install lays down.
type installation struct {
	install       bool
	uninstall     bool
	bin           string
	binNamed      bool
	database      string
	databaseNamed bool
	purge         bool
	assumeYes     bool
}

// installationAsked reads the installation flags off the command line.
//
// flag.Visit walks the flags that were given rather than all of them, which is
// the only way to tell a -db that was left out from one that was named.
func installationAsked(install bool, uninstall bool, bin string, database string, purge bool,
	assumeYes bool) installation {
	asked := installation{
		install:   install,
		uninstall: uninstall,
		bin:       bin,
		database:  database,
		purge:     purge,
		assumeYes: assumeYes,
	}

	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "bin":
			asked.binNamed = true
		case "db":
			asked.databaseNamed = true
		}
	})

	return asked
}

// asked says this command line is an installation command and not a server that
// is starting.
//
// -bin and -purge count, although neither does anything on its own. They are
// refused below rather than ignored: a run that took them for a server start
// would go on serving while the operator waited for an install, and a -purge
// that was quietly dropped would leave somebody believing their data was gone.
func (a installation) asked() bool {
	return a.install || a.uninstall || a.binNamed || a.purge || a.assumeYes
}

// check reports the combinations that are refused, so that they are refused
// before anything is stopped, written or removed.
func (a installation) check() error {
	if a.install && a.uninstall {
		return errors.New("-install and -uninstall ask for opposite things. Give one of them")
	}

	if a.binNamed && !a.install && !a.uninstall {
		return errors.New("-bin names where the executable of an installation is. " +
			"It goes with -install or with -uninstall")
	}

	if a.purge && !a.uninstall {
		return errors.New("-purge removes the data of an installation that is being taken away. " +
			"It goes with -uninstall")
	}

	if a.assumeYes && !a.uninstall {
		return errors.New("-y answers the question -uninstall asks before it removes anything. " +
			"It goes with -uninstall")
	}

	return nil
}

// runInstallation carries out -install or -uninstall and reports the status the
// process ends with.
//
// The reports of what was done are written by the install package itself, whole
// and at the end, so nothing here writes one. What is left for this is the
// refusals and the failures, which go to the error output beside the usage the
// flag package writes there.
func runInstallation(asked installation, in io.Reader, out io.Writer) int {
	err := asked.check()
	if err != nil {
		fmt.Fprintf(flag.CommandLine.Output(), "%v\n\n", err)
		flag.Usage()

		return 1
	}

	if asked.install {
		err = asked.runInstall(out)
	} else {
		err = asked.runUninstall(in, out)
	}

	if err != nil {
		fmt.Fprintf(flag.CommandLine.Output(), "%v\n", err)

		return 1
	}

	return 0
}

// runInstall puts this program in place as a service of this system.
//
// The privilege is checked here as well as inside the install, so that an
// operator who cannot install anything is told so before a release is
// downloaded for an install that is going to be refused.
func (a installation) runInstall(out io.Writer) error {
	err := install.CheckPrivilege()
	if err != nil {
		return err
	}

	plan := install.Defaults()

	// Only what was actually named is taken. A named -db takes the data
	// directory with it, which is WithDatabase's to work out: this process
	// reads the key, the log and the initial password file against the
	// directory the database is in, and an uninstall works that directory out
	// of the registered -db the same way, so a data directory left at the
	// default would be a directory the install made and nothing ever wrote to.
	if a.binNamed {
		plan.ExecutablePath = a.bin
	}

	if a.databaseNamed {
		plan = plan.WithDatabase(a.database)
	}

	dir, err := os.MkdirTemp("", "tunnel-manager-install-")
	if err != nil {
		return fmt.Errorf("failed to make a directory to download the release into: %w", err)
	}

	defer func() {
		_ = os.RemoveAll(dir)
	}()

	fetched, err := install.Fetch(context.Background(), dir)
	if err != nil {
		return err
	}

	defer fetched.Close()

	_, err = install.Install(plan, fetched.Path, installSource(fetched), out)

	return err
}

// runUninstall takes the service of this system away.
//
// -bin and -db are passed only when they were named. The registration is what
// says where this installation is, and what the two flags are for is the
// machine whose registration is already gone; a path this process worked out
// for itself would be a guess reported as if the removal had found it.
func (a installation) runUninstall(in io.Reader, out io.Writer) error {
	err := install.CheckPrivilege()
	if err != nil {
		return err
	}

	removal := install.Removal{Purge: a.purge, AssumeYes: a.assumeYes}

	if a.binNamed {
		removal.ExecutablePath = a.bin
	}

	if a.databaseNamed {
		removal.DatabaseFile = a.database
	}

	_, err = install.Uninstall(removal, in, out)

	return err
}

// installSource is the line the install report says the executable came from.
//
// Which of the two sources was used is the part of an install that cannot be
// seen afterwards from the machine: the file is in place either way, and an
// operator who believes they installed the release when the download failed
// would look for a fix in it that is not there.
func installSource(fetched *install.Fetched) string {
	if fetched.Source == install.SourceRunning {
		text := "this running process, " + fetched.Path

		if fetched.Why != "" {
			text += " (the release was not used: " + fetched.Why + ")"
		}

		return text
	}

	// A release that reaches here was checked against the SHA256SUMS of the
	// release. Fetch leaves on the fallback above whenever there was nothing
	// to check the download against - a release without that file, or without
	// an entry for this one in it - so a binary that came from a release is
	// one whose checksum matched.
	return "the " + fetched.Tag + " release, checksum verified"
}

// usage is what -help prints and what an unknown flag prints. It says where the
// settings are because this text is the only place left that can: there is no
// configuration file any more, and somebody who goes looking for one has
// nothing else to read.
func usage() {
	out := flag.CommandLine.Output()

	fmt.Fprintf(out, "tunnel-manager keeps SSH tunnels to the registered hosts up and serves the API "+
		"and the web UI that manage them.\n\n")
	fmt.Fprintf(out, "Usage: %s [flags]\n\n", filepath.Base(os.Args[0]))
	fmt.Fprintf(out, "Flags:\n")

	flag.PrintDefaults()

	fmt.Fprintf(out, "\nThere is no configuration file. The database file is the whole of this "+
		"installation:\nevery other setting is kept in it and is changed on the Settings screen "+
		"of the web UI.\n")
	fmt.Fprintf(out, "\nRun with -install to have this system start and keep this program running, "+
		"and with\n-uninstall to take that away again.\n")
}

// serviceStopped is closed when the service control manager asks this process
// to stop. It is the sibling of the signal channel and is read by the same
// select, so the shutdown a stop runs is the one a signal runs.
//
// It is out here rather than inside the run because its two ends are apart: the
// function that closes it is handed to the service manager before the run is
// started, and what waits on it is inside the run. The close is guarded the way
// the uninstall and the restart ones are, since the service manager may ask a
// second time while the first shutdown is on its way down.
var serviceStopped = make(chan struct{})

var serviceStopOnce sync.Once

// endOnServiceStop is what the service manager's stop runs.
//
// It returns as soon as the channel is closed. The loop that answers the
// service manager is held up for exactly as long as this takes, and a service
// that stops answering is one the manager kills, so the shutdown itself is left
// to the run this wakes.
func endOnServiceStop() {
	serviceStopOnce.Do(func() {
		close(serviceStopped)
	})
}

// The general information the OpenAPI description carries. It sits above main
// because that is the file `make openapi` points swag at, and swag reads the
// general information from the file it was given and the operations from the
// handlers.
//
// @title        Tunnel Manager API
// @version      3.9.0
// @description  The API the Tunnel Manager screens are built on. Every path
// @description  below sits under /api and needs a session, except POST
// @description  /api/login and GET /api/setup.
// @description
// @description  Logging in sets two cookies. Over HTTPS they are named
// @description  __Host-tm_session and __Host-tm_csrf, and over plain HTTP they
// @description  are tm_session and tm_csrf. A browser sends them back on its
// @description  own, so "Try it out" on this page works as soon as you are
// @description  logged in to the screens in the same browser.
// @description
// @description  Every POST, PUT and DELETE also needs the token of the session
// @description  in an X-CSRF-Token header. POST /api/login answers with it in
// @description  data.csrf_token; paste it into Authorize above and this page
// @description  sends it. A GET needs no token.
// @description
// @description  Every answer has the same shape: {"success":true,"data":...}
// @description  or {"success":false,"error":"..."}. An answer that says no
// @description  carries error_code, which names the refusal and is what a
// @description  script should match on, and error_args, the values its English
// @description  sentence was written with.
//
// The licence is named and not linked. The generated file lands inside the
// directory internal/web embeds, where TestNothingIsFetchedFromTheNetwork holds
// every file to naming nothing outside this server, and the licence is in the
// LICENSE file of the repository anyway.
//
// @license.name  Apache 2.0
//
// @BasePath  /api
//
// @securityDefinitions.apikey  CSRFToken
// @in                          header
// @name                        X-CSRF-Token
// @description                 The token POST /api/login answers with, in
// @description                 data.csrf_token. Required on every POST, PUT
// @description                 and DELETE. The session cookie goes with it and
// @description                 is sent by the browser on its own.
func main() {
	if runningAsService() {
		// Started by a service manager that expects a protocol of it, which is
		// Windows and nowhere else. What the program does is the same; what is
		// around it is the answers the manager waits for, and a process that
		// never gives them is killed for it.
		err := runService(serve, endOnServiceStop)
		if err != nil {
			log.Fatalf("Failed to run under the service manager: %v", err)
		}

		return
	}

	serve()
}

// serve is the whole of this program: it reads the flags, opens the database,
// starts the tunnels and serves the API until it is asked to stop.
//
// It is apart from main so that it can be handed to the service manager on the
// platform that starts it that way. Called from main it is what this program
// has always done from a console.
// logo is the name of the program drawn large. It is what the program puts on
// the terminal first, so that a console someone is watching says what started
// there before any log line does.
const logo = "" +
	"___                 ___\n" +
	" |  |  | |\\ | |\\ | |__  |\n" +
	" |  \\__/ | \\| | \\| |___ |___\n" +
	"                     __   ___  __\n" +
	"|\\/|  /\\  |\\ |  /\\  / _` |__  |__)\n" +
	"|  | /~~\\ | \\| /~~\\ \\__> |___ |  \\"

// printLogo draws the logo with the version beside it. It is skipped for
// -version, whose one line is read by scripts.
func printLogo() {
	fmt.Printf("\n%s  v%s\n\n", logo, version)
}

func serve() {
	flag.Usage = usage

	versionFlag := flag.Bool("version", false, "print the version and exit")
	dbPath := flag.String("db", "",
		"path of the database file, which holds the settings, the registered hosts and the\n"+
			"account. It is created, directories above it included, if it is not there.\n"+
			"Left out, it is "+databaseFileName+" under the directory this platform keeps user\n"+
			"data in (on Linux $XDG_CONFIG_HOME/tunnel-manager, or $HOME/.config/tunnel-manager).")
	resetSettings := flag.Bool("reset-settings", false,
		"put every stored setting back to its default and exit. It is the way out of a\n"+
			"stored setting that keeps the server from starting, since the Settings screen\n"+
			"that would change it is served by the server that will not start.\n"+
			"Only the settings go back. The registered hosts, the service ports, the account\n"+
			"and the certificate are left as they are.")
	installFlag := flag.Bool("install", false,
		"install this program as a service of this system and exit. The executable is put\n"+
			"in place, the data directory is made and the service is registered to start at\n"+
			"boot and to come back on its own. The binary that is installed is the latest\n"+
			"release, or this running one if the release cannot be reached.\n"+
			"It needs root, or an administrator on Windows. Where things go is -bin and -db.")
	uninstallFlag := flag.Bool("uninstall", false,
		"stop the service, take its registration out, remove the installed executable and\n"+
			"exit. Where the installation is comes from the registration itself, so nothing\n"+
			"has to be named. If the registration is already gone, name what is left with\n"+
			"-bin and -db.\n"+
			"The data is kept. -purge is what removes it.")
	binPath := flag.String("bin", "",
		"path the -install puts the executable at, and what the registered service is\n"+
			"started from. Left out, it is the place this platform keeps programs an\n"+
			"administrator installed (on Linux and macOS /usr/local/bin/tunnel-manager).\n"+
			"It goes with -install or with -uninstall and means nothing on its own.")
	purge := flag.Bool("purge", false,
		"remove the data directory as well, for -uninstall. It cannot be taken back: the\n"+
			"database, the key the stored passwords are sealed with and every host and\n"+
			"credential in it go with it.\n"+
			"Without this an uninstall leaves the data where it is and says where that is.")
	trustProxyHeaders := flag.Bool("trust-proxy-headers", false,
		"believe the X-Forwarded-Proto header of whatever is in front of this server. Turn\n"+
			"it on when a reverse proxy terminates TLS and reaches this server in the clear:\n"+
			"the session cookies are then marked Secure, which the connection this process\n"+
			"sees would not ask for.\n"+
			"It is a flag and not a setting because it describes the deployment around this\n"+
			"process rather than something to change while it runs. Leave it out when\n"+
			"nothing is in front, since the header is one any client can send.")
	assumeYes := flag.Bool("y", false,
		"answer yes to the question -uninstall asks before it removes anything. The list\n"+
			"of what would go is still printed.\n"+
			"An uninstall whose standard input is not a terminal - one run from a script -\n"+
			"has nobody to ask and is refused without this.")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("tunnel-manager v%s\n", version)
		os.Exit(0)
	}

	printLogo()

	// The installation commands are done here and end the process, above
	// everything that opens the database. An install has no use for the
	// database of the installation it is laying down, and opening one first
	// would build it under whoever ran the command rather than where the
	// service is about to be registered to read it from.
	installation := installationAsked(*installFlag, *uninstallFlag, *binPath, *dbPath, *purge, *assumeYes)
	if installation.asked() {
		// The standard input is handed over because the uninstall asks before
		// it removes anything, and whether there is anybody to answer is read
		// off this very file: a terminal is asked, a pipe is refused.
		os.Exit(runInstallation(installation, os.Stdin, os.Stdout))
	}

	// The path of the database file is the one thing this process has to be
	// told. Every other setting is read out of that database further down.
	databaseFile := *dbPath

	if databaseFile == "" {
		var err error

		databaseFile, err = defaultDatabasePath()
		if err != nil {
			log.Fatalf("Failed to work out where the database file goes: %v", err)
		}
	}

	// This logger exists so that the steps below have somewhere to report to.
	// Where the logs belong is itself a setting, and the settings are in the
	// database, so the step that opens it runs before anything is known about
	// the logging. Its core is replaced once the settings are read.
	logger, loggerSwitch := newBootstrapLogger()
	defer func() {
		_ = logger.Sync()
	}()

	logger.Info("Starting tunnel-manager...", logid.StartupStarting.Field(), zap.String("version", version))

	// The database is a file, so there is nothing to wait for. A file that
	// cannot be opened is not a problem that comes right on the next try, and
	// retrying would only delay the message that says which path failed.
	// gormLevel is the handle the level of the logger gorm writes through is
	// changed by. It is held from here to the Settings handler for the same
	// reason logLevel below is: the level is a stored setting, and a change to
	// it has to reach a logger that is already in use.
	// Everything this installation owns sits beside the database file, so the
	// directory holding it is what a stored relative path is read against.
	installDir := filepath.Dir(databaseFile)

	db, gormLevel, err := database.NewDatabase(databaseFile, logger, bootstrapLogLevel)
	if err != nil {
		logger.Fatal("failed to open the database",
			logid.DatabaseOpenFailed.Field(),
			zap.String("path", databaseFile),
			zap.Error(err))
	}

	// The reset runs before the settings are read, because the set it is there
	// to repair is exactly the one a read refuses.
	if *resetSettings {
		resetStoredSettings(db, logger)
	}

	// A path stored while an absolute one was still accepted is put back to
	// its default here, above the read that would refuse it. An installation
	// that was working goes on working, on a path inside its own directory.
	repairStoredPaths(db, logger)

	set, err := settings.Load(db)
	if err != nil {
		logger.Fatal("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
	}

	// From here the logger is the one the settings describe. logLevel is the
	// handle the Settings screen changes the level through, which is why the
	// level is not fixed into the core.
	core, logLevel, emptyLog, err := initLogger(set, installDir)
	if err != nil {
		logger.Fatal("failed to initialize the logger", logid.LoggingInitFailed.Field(), zap.Error(err))
	}

	loggerSwitch.set(core)

	// The database was opened at bootstrapLogLevel, since the level it should
	// run at was inside it. Without this the stored logging.level would reach
	// everything but the statements gorm reports, which is where a query that
	// fails is named.
	//
	// The level goes on the handle rather than on db.Logger, so that the same
	// handle carries every later change as well. Set through db.Logger it would
	// be fixed here once and the Settings screen would have nothing to change.
	gormLevel.Set(set.LoggingLevel)

	logger.Info("read the settings from the database",
		logid.SettingsRead.Field(),
		zap.String("log_level", logLevel.String()),
		zap.String("log_format", set.LoggingFormat),
		zap.String("log_file", resolveInstallPath(installDir, set.LoggingFilePath)),
		zap.Int("api_port", set.APIPort),
		zap.Int("monitoring_interval_sec", set.MonitoringIntervalSec),
		zap.Int("reconcile_interval_sec", set.ReconcileIntervalSec))

	warnIfNotPrivileged(logger)

	checkUlimit(logger)

	// Without the key no stored password can be read, so a key that cannot be
	// loaded stops the startup instead of leaving every tunnel unable to connect.
	keyFile := resolveInstallPath(installDir, set.SecurityKeyFile)

	key, err := crypto.LoadOrCreateKey(keyFile)
	if err != nil {
		logger.Fatal("failed to load the encryption key",
			logid.EncryptionKeyLoadFailed.Field(),
			zap.String("key_file", keyFile),
			zap.Error(err))
	}

	cipher, err := crypto.NewCipher(key)
	if err != nil {
		logger.Fatal("failed to initialize the encryption", logid.EncryptionInitFailed.Field(), zap.Error(err))
	}
	// The path is reported as an absolute one. The configured value may be
	// relative, and a relative path is read against the working directory,
	// which differs between running from the repository, from the container and
	// from systemd. Logging it as it was written tells the operator nothing
	// about which file was actually opened.
	keyPath, err := filepath.Abs(keyFile)
	if err != nil {
		keyPath = keyFile
	}

	logger.Info("loaded the encryption key", logid.EncryptionKeyLoaded.Field(), zap.String("path", keyPath))

	// The signals are taken over before anything that has to be torn down is
	// built, because until they are the default disposition kills the process
	// and leaves the tunnels behind. The same channel is read by the shutdown
	// at the end of main.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	// The stored passwords are read before any tunnel is built, so a key that
	// opens none of them stops the startup here instead of letting every Host
	// fail one SSH attempt at a time behind an API that answers normally.
	checkEncryptionKey(db, cipher, logger, set.SecurityKeyFile)

	// The account is set up before anything is served and before any tunnel is
	// built. The table it reads is created by the migration that NewDatabase
	// runs, and a failure here stops the startup while there is nothing to tear
	// down. Leaving it to a later point would let the API come up with no
	// account to authenticate against.
	// The path is worked out once and handed to both the startup, which writes
	// the file, and the setup, which deletes it once the account is settled. It
	// sits beside the database file: that directory is where this installation
	// keeps its data, the process already writes to it, and it exists by now
	// since opening the database made it.
	initialPasswordFile := auth.InitialPasswordFile(databaseFile)

	ensureUser(db, logger, initialPasswordFile)

	manager, err := tunnel.NewManager(db, logger, cipher, set.MonitoringIntervalSec)
	if err != nil {
		logger.Fatal("failed to create the tunnel manager", logid.TunnelManagerCreateFailed.Field(), zap.Error(err))
	}

	// The first reconcile pass runs before anything is served, so the tunnels
	// of the rows that are already stored are up by the time the first request
	// can ask about them.
	logger.Info("Restoring all tunnels...", logid.TunnelRestoreStarting.Field())
	err = manager.RestoreAllTunnels()
	if err != nil {
		logger.Error("failed to restore tunnels", logid.TunnelRestoreFailed.Field(), zap.Error(err))
	}

	// From here on the loop is the only thing that starts and stops tunnels. The
	// handlers write rows and wake it up. reconcileDone reports that it returned,
	// because stopping the tunnels while it still runs would let it start them
	// again.
	reconcileCtx, stopReconcile := context.WithCancel(context.Background())
	reconcileDone := make(chan struct{})

	go func() {
		defer close(reconcileDone)
		manager.RunReconcileLoop(reconcileCtx, set.ReconcileIntervalSec)
	}()

	// stopReconcileLoop ends the loop and waits for the pass it is in to end.
	// Both the shutdown at the bottom of main and the uninstall stop it through
	// this one function, because both go on to stop the tunnels and a loop that
	// is still running starts them again.
	//
	// The wait is given up after reconcileStopTimeout: a pass that is still
	// dialing holds nothing that survives this process, which is on its way out
	// either way.
	stopReconcileLoop := func() {
		stopReconcile()

		select {
		case <-reconcileDone:
		case <-time.After(reconcileStopTimeout):
			logger.Warn("the reconcile loop did not return in time, stopping the tunnels anyway",
				logid.TunnelReconcileStopTimedOut.Field(),
				zap.Duration("waited", reconcileStopTimeout))
		}
	}

	// uninstalled is what the uninstall ends the process through. It removed
	// the files and left the browser the seconds it needs to draw the answer by
	// the time this is closed, and what follows is the same shutdown a signal
	// runs, so the API is taken down in order and the logs are flushed.
	//
	// The close is guarded because a second uninstall would otherwise panic on
	// a channel that is already closed. Nothing is left for it to do anyway:
	// the first one closed the database, so it cannot even check a password.
	uninstalled := make(chan struct{})

	var uninstallOnce sync.Once

	endAfterUninstall := func() {
		uninstallOnce.Do(func() {
			close(uninstalled)
		})
	}

	// restarting is what the Settings screen asks for a restart through. It is
	// the sibling of the channel above and is read by the same select, so a
	// restart runs the very shutdown a signal runs: the API is drained, the
	// port is released, the reconcile loop is stopped and the tunnels come
	// down. Only once all of that has ended does this process run this program
	// again, because the image that replaces it binds the port this one is
	// serving on.
	//
	// The close is guarded for the same reason the one above is: a second press
	// while the first restart is on its way down would panic on a channel that
	// is already closed.
	restarting := make(chan struct{})

	var restartOnce sync.Once

	endBeforeRestart := func() {
		restartOnce.Do(func() {
			close(restarting)
		})
	}

	e := echo.New()
	// The framework draws a banner of its own and announces the port it
	// listens on. The logo above is what this program says for itself.
	e.HideBanner = true
	e.HidePort = true
	e.Validator = &CustomValidator{validator: validator.New()}
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	// There is no CORS middleware on purpose. The UI is built into this binary
	// and served from /ui/, so every call it makes is same-origin and needs no
	// grant from one. With no Access-Control-Allow-Origin header in the answer,
	// a browser refuses to hand any page on another origin what this API said,
	// and it refuses the preflight that a cross-origin request carrying the
	// CSRF header would need. Clients that are not browsers, curl and scripts
	// among them, are untouched: CORS is a rule browsers apply to pages, not a
	// check this server performs.

	h := api.NewHandler(db, manager, logger, cipher)
	authHandler := api.NewAuthHandler(db, logger, initialPasswordFile)
	// The holder is what the certificate is replaced through while the process
	// runs. It is built empty and here, above both the routes and the listener,
	// because the handler that replaces the certificate and the listener that
	// serves it have to be looking at the same one. It stays empty while HTTPS
	// is off, and the handler answers with what that means rather than with a
	// certificate nothing is serving.
	certHolder := tlsserve.NewHolder(nil)

	// The body limits, the security headers and the server deadlines. They are
	// put on here rather than beside the two lines above because the headers
	// read the certificate being served, and the holder that answers for it
	// has to exist first. echo applies what e.Use carries at the time a request
	// is served and not at the time a route is added, so this reaches the
	// routes below and the UI alike.
	harden(e, certHolder.Current)

	certificateHandler := api.NewCertificateHandler(db, cipher, logger, certHolder)
	// The level handle goes to the handler that stores the settings, so that a
	// stored logging.level reaches the running loggers as it is saved. It is
	// the one setting this process can take on without being started again.
	//
	// The settings this startup read go with it, so that a read can hold them
	// against what is stored and name the settings a restart is still owed for.
	// Dropped here, the only place that knew would be the answer to the save
	// that stored them, which is gone as soon as the screen is left.
	settingsHandler := api.NewSettingsHandler(db, logger, logLevel, gormLevel, *set, installDir)
	// The log screen is handed the path this process resolved, the same one the
	// logger above writes through. Worked out on the screen instead it would be
	// a second place that knows what a relative logging.file.path is read
	// against, and a path changed on the Settings screen without a restart
	// would send the screen to a file nothing is being written to.
	logsHandler := api.NewLogsHandler(logger, resolveInstallPath(installDir, set.LoggingFilePath),
		db, emptyLog)
	// The uninstall is handed what this process holds: the manager whose
	// tunnels have to come down, the function that stops the loop that would
	// build them again, the database handle it closes and the paths of the
	// files this installation is made of. The paths are the ones the startup
	// opened, so what goes is what was in use and not what a setting saved
	// without a restart names.
	uninstallHandler := api.NewUninstallHandler(db, logger, manager, stopReconcileLoop,
		api.UninstallPaths{
			DatabaseFile:        databaseFile,
			KeyFile:             keyFile,
			InitialPasswordFile: initialPasswordFile,
			LogFile:             resolveInstallPath(installDir, set.LoggingFilePath),
		}, endAfterUninstall)
	// The restart is handed the shutdown above and what this build can do about
	// coming back. canReexec is asked here, in the one place that knows it is
	// the process, rather than in the handler: the handler answers with it, and
	// a screen that had to work it out would be guessing from the browser it
	// runs in.
	restartHandler := api.NewRestartHandler(logger, canReexec(), endBeforeRestart)
	// The update screen is handed what it needs to look and what it needs to
	// install. Both are functions rather than the packages themselves, so that
	// a test drives the handler without reaching GitHub or replacing anything.
	updateHandler := api.NewUpdateHandler(logger, db, version, canInstallUpdate(databaseFile),
		install.Check, func() error {
			return startUpdateInstall(logger, databaseFile)
		})
	// The export and the import are handed the handler that serves the Hosts,
	// because an imported Host has to be stored the way a created one is. The
	// version goes into the file, so that a file found later says what wrote it.
	transferHandler := api.NewTransferHandler(h, version)
	// The forwarding headers are believed only where -trust-proxy-headers said
	// so, and this is settled here, above the routes, so that nothing is
	// serving while the answer is written.
	applyProxyTrust(authHandler, *trustProxyHeaders)
	g := e.Group(apiPrefix)

	// The session check is put on the group before any route is added to it.
	// echo binds the middleware a group carries at the time the route is added,
	// so a route added first would be served without it. The UI is registered
	// on the instance instead, and stays outside this check on purpose: the
	// login screen is served from there and would otherwise be behind the very
	// login it is there to offer.
	g.Use(authHandler.RequireSession())

	g.POST("/login", authHandler.Login)
	g.POST("/logout", authHandler.Logout)
	g.GET("/setup", authHandler.GetSetup)
	g.POST("/setup", authHandler.Setup)

	g.GET("/account", authHandler.GetAccount)
	g.PUT("/account", authHandler.ChangeAccount)

	g.POST("/host", h.CreateHost)
	g.GET("/host", h.ListHosts)
	g.GET("/host/:id", h.GetHost)
	g.PUT("/host/:id", h.UpdateHost)
	g.DELETE("/host/:id", h.DeleteHost)

	// Approving the host key of a Host is a change to the Host, so it hangs
	// under it and behind the same session check. It is a POST and carries a
	// body, because a Host whose trusted key is being replaced is approved
	// with the password of the account, and a password in a URL is written to
	// the access log of this server and to the history of the browser that
	// sent it.
	g.POST("/host/:id/host-key", h.ApproveHostKey)

	// The keys waiting to be approved across every Host, read a page at a time
	// and approved together. It is its own path and not a filter on /host,
	// because what a row of it carries is the pair of fingerprints and nothing
	// else of the Host, and because the press over it writes to several Hosts
	// at once and so belongs to no single one of them. The POST carries a body
	// for the reason the one above it does.
	g.GET("/host-key", h.ListHostKeysWaiting)
	g.POST("/host-key", h.ApproveHostKeys)

	// The service ports a Host carries are read and changed under the Host, on
	// the group that carries the session check, because an assignment decides
	// which tunnels this installation runs.
	g.GET("/host/:id/service-port", h.ListHostServicePorts)
	g.PUT("/host/:id/service-port", h.UpdateHostServicePorts)

	g.POST("/service-port", h.CreateServicePort)
	g.GET("/service-port", h.ListServicePorts)
	g.GET("/service-port/:id", h.GetServicePort)
	g.PUT("/service-port/:id", h.UpdateServicePort)
	g.DELETE("/service-port/:id", h.DeleteServicePort)

	g.GET("/status", h.GetStatus)
	g.GET("/status/:hostId", h.GetHostStatus)

	g.GET("/settings", settingsHandler.GetSettings)
	g.PUT("/settings", settingsHandler.UpdateSettings)

	// The exports are POST because the password that seals the file is in the
	// body. A password in a URL is written to the access log of this server and
	// to the history of the browser that asked for it.
	//
	// The two imports carry a body limit of their own, since what they are
	// given is a whole exported configuration and the general limit is sized
	// for a request that carries a form: see importBodyLimit. The route-level
	// middleware is what isImportRoute leaves room for by skipping these two
	// paths on the instance.
	g.POST("/export/tunnels", transferHandler.ExportTunnels)
	g.POST(importTunnelsPath, transferHandler.ImportTunnels, importBodyLimitMiddleware())
	g.POST("/export/settings", transferHandler.ExportSettings)
	g.POST(importSettingsPath, transferHandler.ImportSettings, importBodyLimitMiddleware())

	g.GET("/certificate", certificateHandler.GetCertificate)
	g.POST("/certificate/renew", certificateHandler.RenewCertificate)
	g.PUT("/certificate", certificateHandler.InstallCertificate)

	g.GET("/logs", logsHandler.GetLogs)
	// The emptying is a POST and not a DELETE on the path above because the
	// password of the account is in the body. It is the shape the uninstall and
	// the exports are on, and for the same reason.
	g.POST("/logs/clear", logsHandler.ClearLogs)

	g.GET("/restart", restartHandler.GetRestart)
	g.POST("/restart", restartHandler.Restart)

	g.POST("/uninstall", uninstallHandler.Uninstall)

	// The look for a newer release runs on the same context the reconcile loop
	// does, so the shutdown that stops one stops the other. It is started after
	// the handler exists because both of them keep the one answer.
	go runUpdateChecks(reconcileCtx, logger, db, updateHandler, canInstallUpdate(databaseFile))

	g.GET("/update", updateHandler.GetUpdate)
	// Both of these are POST rather than GET. The check makes a request to
	// another host, and the install replaces the executable; neither is a thing
	// a link or a prefetch may set off.
	g.POST("/update/check", updateHandler.CheckUpdate)
	g.POST("/update/install", updateHandler.InstallUpdate)

	// The UI is put on the instance itself and not on the group above. It is
	// the same bytes for every client and carries no data of its own, while
	// everything it shows comes from /api/**, which stays behind the session.
	web.RegisterRoutes(e, version)

	// The API documentation goes on beside it, on the instance and not on the
	// group, so that it is reachable without a session: it describes the login
	// call among the rest, and a client reading it to find out how to log in
	// has none yet. The policy it needs is built here, where every other
	// header this server sends is decided, and it reaches that prefix alone.
	web.RegisterAPIDocs(e, apiDocsContentSecurityPolicy)

	// A server that never comes up must not end the process on the spot. The
	// tunnels are restored by now and their rows are in the database, and
	// logger.Fatal would leave both behind.
	serverErr := make(chan error, 1)

	address := fmt.Sprintf(":%d", set.APIPort)

	// While HTTPS is on there is still one port, and two servers behind it.
	// portSplit accepts on the port and sorts the connections by their first
	// byte: the ones that opened a TLS handshake go to echo, and everything
	// else goes to redirectServer, which answers with the same address under
	// https and reads no request body.
	//
	// Both are held here because the shutdown at the bottom of main has to take
	// them down as well, and they are nil while HTTPS is off.
	var portSplit *tlsserve.Splitter

	var redirectServer *http.Server

	if set.APIHTTPSEnabled {
		cert, certInfo, certErr := tlsserve.LoadOrCreate(db, cipher, time.Now())
		if certErr != nil {
			// Carrying on in the clear is not an option here. The operator
			// turned HTTPS on, the screens send the password of the account,
			// and a server that quietly fell back would put it on the wire
			// under an address that says https in nobody's browser.
			logger.Fatal("failed to prepare the TLS certificate. Start with api.https_enabled turned "+
				"off to serve in the clear while this is sorted out",
				logid.CertificatePrepareFailed.Field(),
				zap.Error(certErr))
		}

		if certInfo.Created {
			logger.Info("generated a certificate for this installation and stored it in the database. "+
				"Nobody signed for it, so a client shows a warning until the certificate is trusted on "+
				"that machine. The fingerprint is what to check it against",
				logid.CertificateGenerated.Field(),
				zap.String("reason", certInfo.Reason),
				zap.String("fingerprint_sha256", certInfo.Fingerprint),
				zap.Strings("hosts", certInfo.Hosts),
				zap.Time("not_after", certInfo.NotAfter))
		} else {
			logger.Info("read the certificate of this installation from the database",
				logid.CertificateReadFromDatabase.Field(),
				zap.String("fingerprint_sha256", certInfo.Fingerprint),
				zap.Strings("hosts", certInfo.Hosts),
				zap.Time("not_after", certInfo.NotAfter))
		}

		// The port is taken here rather than inside the server, so that a port
		// which is already in use is reported through the channel the startup
		// below watches. That is where it was reported from before, and it is
		// what leaves the tunnels to be taken down in order.
		listener, listenErr := net.Listen("tcp", address)
		if listenErr != nil {
			serverErr <- fmt.Errorf("failed to listen on %s: %w", address, listenErr)
		} else {
			portSplit = tlsserve.NewSplitter(listener, logger)
			redirectServer = tlsserve.NewRedirectServer(set.APIPort, certHolder, logger)

			// echo is handed a listener that is already wrapped in TLS, which
			// is what it does for itself in StartTLS. It keeps its own server
			// and with it the graceful shutdown the rest of main relies on.
			certHolder.Set(cert)

			e.TLSServer.TLSConfig = tlsserve.ServerConfig(certHolder)
			e.TLSListener = tls.NewListener(portSplit.TLS(), e.TLSServer.TLSConfig)

			go func() {
				err := redirectServer.Serve(portSplit.Plain())
				// A closed listener is how this server ends every time: the
				// shutdown closes the port out from under it.
				if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
					logger.Error("the server that redirects plaintext requests stopped",
						logid.ApiServerRedirectServerStopped.Field(),
						zap.Error(err))
				}
			}()

			go func() {
				err := e.StartServer(e.TLSServer)
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					serverErr <- err
				}
			}()

			logger.Info("serving the API and the web UI over HTTPS. A request that arrives in the clear "+
				"on the same port is answered with a redirect to https",
				logid.ApiServerServingHttps.Field(),
				zap.String("address", listener.Addr().String()),
				zap.Int("port", set.APIPort))
		}
	} else {
		// The port is taken here rather than inside the server for the same
		// reason as above, and so that the line below can name the address the
		// listener bound instead of the one that was asked for. echo serves on
		// a listener that is already open when it is handed one.
		listener, listenErr := net.Listen("tcp", address)
		if listenErr != nil {
			serverErr <- fmt.Errorf("failed to listen on %s: %w", address, listenErr)
		} else {
			e.Listener = listener

			go func() {
				err := e.Start(address)
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					serverErr <- err
				}
			}()

			logger.Warn("HTTPS is turned off, so the API and the web UI are served in the clear. Everything "+
				"the screens send travels as it is, the password of the account among it",
				logid.ApiServerServingPlain.Field(),
				zap.String("address", listener.Addr().String()),
				zap.Int("port", set.APIPort))
		}
	}

	var startErr error

	// restartAsked separates the one shutdown that is not the end of this
	// process from the others. What follows is the same for all of them, and
	// what it is followed by is not.
	restartAsked := false

	select {
	case sig := <-sigChan:
		logger.Info("Received signal, shutting down...",
			logid.ShutdownSignalReceived.Field(),
			zap.String("signal", sig.String()))
	case <-serviceStopped:
		// Reported under the signal id. A stop from the service manager is what
		// a signal is on the platform that has no signals, and the shutdown it
		// asks for is the same one.
		logger.Info("The service manager asked for a stop, shutting down...",
			logid.ShutdownSignalReceived.Field(),
			zap.String("signal", "service manager stop"))
	case <-uninstalled:
		logger.Info("The installation was removed, shutting down...", logid.ShutdownUninstalled.Field())
	case <-restarting:
		restartAsked = true

		logger.Info("A restart was asked for, shutting down before this program is run again...",
			logid.ShutdownRestartAsked.Field())
	case startErr = <-serverErr:
		logger.Error("failed to start API server", logid.ApiServerStartFailed.Field(), zap.Error(startErr))
	}

	// The API server goes down first so that no request observes tunnels
	// being torn down underneath it. A server that failed to bind has nothing
	// left to serve and answers right away.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = e.Shutdown(ctx)
	if err != nil {
		logger.Error("failed to shut down API server gracefully", logid.ApiServerShutdownFailed.Field(), zap.Error(err))
	}

	// The redirect server is drained the same way, so a browser that was being
	// sent to https gets its answer rather than a reset connection.
	if redirectServer != nil {
		err = redirectServer.Shutdown(ctx)
		if err != nil {
			logger.Error("failed to shut down the redirect server gracefully",
				logid.ApiServerRedirectShutdownFailed.Field(),
				zap.Error(err))
		}
	}

	// Closing the API server closed the port already, since the listener it was
	// given is one side of the split. This is here for the startup that built
	// the split and never got to serve on it, and closing what is closed costs
	// nothing.
	if portSplit != nil {
		err = portSplit.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Error("failed to close the API port", logid.ApiServerPortCloseFailed.Field(), zap.Error(err))
		}
	}

	// The loop is stopped before the tunnels are, because it starts again what
	// is stopped while it runs. An uninstall stopped both already, and stopping
	// what is stopped costs nothing.
	stopReconcileLoop()

	logger.Info("Stopping all tunnels...", logid.ShutdownStoppingTunnels.Field())
	manager.StopAllTunnels()

	// The restart comes last of all. Everything above it is what a signal does
	// too, and every one of those steps is one the image that replaces this
	// process needs to have finished: the port above all, since a listener that
	// is still open is one the new image cannot bind.
	if restartAsked {
		finishRestart(logger)
	}

	logger.Info("Exiting tunnel-manager...", logid.ShutdownExiting.Field())

	if startErr != nil {
		// os.Exit does not run the deferred Sync, so the logs are flushed here.
		_ = logger.Sync()
		os.Exit(1)
	}
}

// finishRestart runs this program again in place of this process.
//
// It is called only once the ordered shutdown above has ended. The port is free
// by then, which is the one thing the image that replaces this process cannot
// do without: it binds the same port a moment later.
//
// A platform without exec ends here instead. The process goes down in order and
// what starts it again, if anything does, is whatever supervises it. The screen
// was told which of the two this is before the operator pressed anything.
func finishRestart(logger *zap.Logger) {
	if !canReexec() {
		logger.Info("the restart ends with this process. This platform cannot replace the "+
			"image of a running process, so starting this program again is left to whatever "+
			"supervises this service",
			logid.RestartEndsWithThisProcess.Field())

		return
	}

	logger.Info("the shutdown has ended and the port is free, so this process now runs this "+
		"program again with the arguments and the environment it was started with",
		logid.RestartRunningAgain.Field())

	// exec replaces the image of this process and runs no deferred call, so
	// what has been logged is written out here. Without this the last lines of
	// this process, the one above among them, are lost in the buffer.
	_ = logger.Sync()

	err := reexec()

	// Only a failure comes back from that call. The logger is still the one of
	// this process, since nothing was replaced.
	logger.Error("failed to run this program again, so this process ends instead. A service "+
		"that is supervised is started again from here; one that was started by hand is not",
		logid.RestartExecFailed.Field(),
		zap.Error(err))

	// The exit code says the process did not end the way it meant to, which is
	// what a supervisor reads to decide whether to start it again.
	_ = logger.Sync()
	os.Exit(1)
}
