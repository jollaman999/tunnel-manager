package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
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

const version = "3.5.0"

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

// prepareLogFile makes sure the configured log file can be written to.
func prepareLogFile(s *settings.Settings, installDir string) error {
	logFilePath := resolveInstallPath(installDir, s.LoggingFilePath)
	logDir := filepath.Dir(logFilePath)
	err := os.MkdirAll(logDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}

	file, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("failed to create log file: %v", err)
	}

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
func initLogger(s *settings.Settings, installDir string) (zapcore.Core, zap.AtomicLevel, error) {
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
		return nil, level, fmt.Errorf("failed to parse log level: %v", err)
	}

	encoderConfig := newEncoderConfig()

	var encoder zapcore.Encoder
	if s.LoggingFormat == "json" {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	var cores []zapcore.Core

	if fileErr == nil {
		logWriter := &lumberjack.Logger{
			Filename:   resolveInstallPath(installDir, s.LoggingFilePath),
			MaxSize:    s.LoggingFileMaxSize,
			MaxBackups: s.LoggingFileMaxBackups,
			MaxAge:     s.LoggingFileMaxAge,
			Compress:   s.LoggingFileCompress,
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

	return core, level, nil
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
}

func main() {
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
	flag.Parse()

	if *versionFlag {
		fmt.Printf("tunnel-manager v%s\n", version)
		os.Exit(0)
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

	set, err := settings.Load(db)
	if err != nil {
		logger.Fatal("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
	}

	// From here the logger is the one the settings describe. logLevel is the
	// handle the Settings screen changes the level through, which is why the
	// level is not fixed into the core.
	core, logLevel, err := initLogger(set, installDir)
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
	logsHandler := api.NewLogsHandler(logger, resolveInstallPath(installDir, set.LoggingFilePath))
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
	// The export and the import are handed the handler that serves the Hosts,
	// because an imported Host has to be stored the way a created one is. The
	// version goes into the file, so that a file found later says what wrote it.
	transferHandler := api.NewTransferHandler(h, version)
	g := e.Group("/api")

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
	g.POST("/export/tunnels", transferHandler.ExportTunnels)
	g.POST("/import/tunnels", transferHandler.ImportTunnels)
	g.POST("/export/settings", transferHandler.ExportSettings)
	g.POST("/import/settings", transferHandler.ImportSettings)

	g.GET("/certificate", certificateHandler.GetCertificate)
	g.POST("/certificate/renew", certificateHandler.RenewCertificate)
	g.PUT("/certificate", certificateHandler.InstallCertificate)

	g.GET("/logs", logsHandler.GetLogs)

	g.GET("/restart", restartHandler.GetRestart)
	g.POST("/restart", restartHandler.Restart)

	g.POST("/uninstall", uninstallHandler.Uninstall)

	// The UI is put on the instance itself and not on the group above. It is
	// the same bytes for every client and carries no data of its own, while
	// everything it shows comes from /api/**, which stays behind the session.
	web.RegisterRoutes(e, version)

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
			redirectServer = tlsserve.NewRedirectServer(set.APIPort, logger)

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
				zap.Int("port", set.APIPort))
		}
	} else {
		logger.Warn("HTTPS is turned off, so the API and the web UI are served in the clear. Everything "+
			"the screens send travels as it is, the password of the account among it",
			logid.ApiServerServingPlain.Field(),
			zap.Int("port", set.APIPort))

		go func() {
			err := e.Start(address)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		}()
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
