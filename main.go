package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gorm.io/gorm"

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/config"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/database"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/jollaman999/tunnel-manager/internal/web"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const version = "2.0.2"

// Maximum time to wait for in-flight HTTP requests to finish on shutdown.
const shutdownTimeout = 10 * time.Second

// Maximum time to wait for the reconcile loop to return on shutdown. The loop
// leaves as soon as the pass it is running ends, and a pass that is dialing a
// Host that does not answer takes as long as the SSH timeouts of the tunnels it
// still has to start. Waiting for it without a bound would hold the process up
// long after the API stopped answering.
const reconcileStopTimeout = 10 * time.Second

// errShutdownRequested reports that the wait for the database ended because a
// shutdown signal arrived, not because the database answered.
var errShutdownRequested = errors.New("shutdown requested while waiting for the database")

func initDatabase(cfg *config.Config, logger *zap.Logger, sigChan <-chan os.Signal) (*gorm.DB, error) {
	timeout := time.After(time.Duration(cfg.Database.TimeoutSec) * time.Second)
	tick := time.Tick(1 * time.Second)

	for {
		select {
		case sig := <-sigChan:
			// The signals are already delivered to the channel at this point,
			// so the wait has to read it. Leaving it to the shutdown path at
			// the end of main would let the signal sit in the buffer and keep
			// the process up until the database answers or the wait times out.
			logger.Info("Received signal while waiting for the database, shutting down...", zap.String("signal", sig.String()))
			return nil, errShutdownRequested
		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for database connection after %s seconds", strconv.Itoa(cfg.Database.TimeoutSec))
		case <-tick:
			db, err := database.NewDatabase(cfg.Database.Host, cfg.Database.Port, cfg.Database.User, cfg.Database.Password, cfg.Database.Name, logger, cfg.Logging.Level)
			if err != nil {
				logger.Info("attempting to connect to database...", zap.String("host", cfg.Database.Host), zap.Int("port", cfg.Database.Port), zap.Error(err))
				continue
			}
			logger.Info("successfully connected to database")
			return db, nil
		}
	}
}

// prepareLogFile makes sure the configured log file can be written to.
func prepareLogFile(cfg *config.Config) error {
	logDir := filepath.Dir(cfg.Logging.File.Path)
	err := os.MkdirAll(logDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create log directory: %v", err)
	}

	logFile := cfg.Logging.File.Path
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
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

func initLogger(cfg *config.Config) (*zap.Logger, error) {
	// An unusable log file is not fatal. The logger falls back to the console
	// only, but the reason has to be visible since nothing is written to the file.
	fileErr := prepareLogFile(cfg)
	if fileErr != nil {
		log.Printf("Logging to file is disabled: %v (path: %s)", fileErr, cfg.Logging.File.Path)
	}

	var level zapcore.Level
	err := level.UnmarshalText([]byte(cfg.Logging.Level))
	if err != nil {
		return nil, fmt.Errorf("failed to parse log level: %v", err)
	}

	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	var encoder zapcore.Encoder
	if cfg.Logging.Format == "json" {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	var cores []zapcore.Core

	if fileErr == nil {
		logWriter := &lumberjack.Logger{
			Filename:   cfg.Logging.File.Path,
			MaxSize:    cfg.Logging.File.MaxSize,
			MaxBackups: cfg.Logging.File.MaxBackups,
			MaxAge:     cfg.Logging.File.MaxAge,
			Compress:   cfg.Logging.File.Compress,
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

	logger := zap.New(zapcore.NewTee(cores...), zap.AddCaller())

	if fileErr != nil {
		logger.Warn("logging to file is disabled",
			zap.String("path", cfg.Logging.File.Path),
			zap.Error(fileErr),
			zap.String("message", "logs are written to the console only"))
	}

	return logger, nil
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
		logger.Warn("failed to read the Hosts to check the encryption key against", zap.Error(err))
		return
	}

	check := checkStoredPasswords(hosts, cipher)

	if check.keyIsWrong() {
		logger.Fatal("the configured key file opens none of the stored passwords, so it is not the key "+
			"they were encrypted with. Put the key file that the passwords were stored with back in place, "+
			"or set the password of every Host again through the API. Starting against another key is "+
			"refused because no tunnel could be built and no stored password could be read",
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
			zap.String("key_file", keyFile),
			zap.Int("hosts_that_do_not_open", len(check.hostsThatDoNotOpen)),
			zap.Uints("host_ids_that_do_not_open", check.hostsThatDoNotOpen),
			zap.Int("hosts_that_open", check.decrypted))
	}

	switch {
	case check.decrypted > 0:
		logger.Info("the encryption key opens the stored passwords",
			zap.String("key_file", keyFile),
			zap.Int("hosts_that_open", check.decrypted))
	case len(check.hostsThatDoNotOpen) == 0:
		logger.Info("no stored password is encrypted, so there is nothing to check the encryption key against",
			zap.Int("hosts", len(hosts)))
	}
}

// ensureUser creates the single account on the first startup. The initial
// password is written to a file next to the configuration file and never to the
// log, which goes to the console as well as to the log file, so only the path is
// reported. The encryption key file is handled the same way.
func ensureUser(db *gorm.DB, logger *zap.Logger, passwordFile string) {
	created, err := auth.EnsureUser(db, passwordFile)
	if err != nil {
		// The startup stops here rather than going on with a warning. Nothing
		// can log in while the account is missing, so an API that came up would
		// answer nobody, and a directory that cannot be written to is a
		// deployment question the operator has to settle once: point -config at
		// a directory the process may write to, or give it that permission.
		logger.Fatal("failed to set up the account. The initial password is written next to the "+
			"configuration file, so the process has to be allowed to write to that directory",
			zap.Error(err),
			zap.String("initial_password_file", passwordFile))
	}

	if created {
		logger.Info("created the account with an initial password. Read the password from the file, log in "+
			"with it, and set a username and a password. The file is written with permission 0600 and holds "+
			"the only copy of the password",
			zap.String("initial_password_file", passwordFile))
		return
	}

	logger.Info("the account is already set up")
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

func main() {
	versionFlag := flag.Bool("version", false, "show the version and exit")
	configPath := flag.String("config", "config/config.yaml", "path to config file")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("tunnel-manager v%s\n", version)
		os.Exit(0)
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	logger, err := initLogger(cfg)
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer func() {
		_ = logger.Sync()
	}()

	logger.Info("Starting tunnel-manager...")

	warnIfNotPrivileged(logger)

	checkUlimit(logger)

	// Without the key no stored password can be read, so a key that cannot be
	// loaded stops the startup instead of leaving every tunnel unable to connect.
	key, err := crypto.LoadOrCreateKey(cfg.Security.KeyFile)
	if err != nil {
		log.Fatalf("Failed to load the encryption key: %v", err)
	}

	cipher, err := crypto.NewCipher(key)
	if err != nil {
		log.Fatalf("Failed to initialize the encryption: %v", err)
	}
	// The path is reported as an absolute one. The configured value may be
	// relative, and a relative path is read against the working directory,
	// which differs between running from the repository, from the container and
	// from systemd. Logging it as it was written tells the operator nothing
	// about which file was actually opened.
	keyPath, err := filepath.Abs(cfg.Security.KeyFile)
	if err != nil {
		keyPath = cfg.Security.KeyFile
	}

	logger.Info("loaded the encryption key", zap.String("path", keyPath))

	// The signals are taken over before the wait for the database, which runs
	// for as long as the configured timeout allows. Until they are, the default
	// disposition kills the process, and nothing that was set up above is torn
	// down. One channel serves both this wait and the shutdown at the end of
	// main: only one of the two reads it, because a signal that arrives here
	// ends the startup.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	db, err := initDatabase(cfg, logger, sigChan)
	if err != nil {
		if errors.Is(err, errShutdownRequested) {
			// No tunnel and no manager exist yet, so the logger is all there is
			// to flush, and a shutdown that was asked for is not a failure.
			logger.Info("Exiting tunnel-manager...")
			return
		}
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// The stored passwords are read before any tunnel is built, so a key that
	// opens none of them stops the startup here instead of letting every Host
	// fail one SSH attempt at a time behind an API that answers normally.
	checkEncryptionKey(db, cipher, logger, cfg.Security.KeyFile)

	// The account is set up before anything is served and before any tunnel is
	// built. The table it reads is created by the migration that initDatabase
	// runs, and a failure here stops the startup while there is nothing to tear
	// down. Leaving it to a later point would let the API come up with no
	// account to authenticate against.
	// The path is worked out once and handed to both the startup, which writes
	// the file, and the setup, which deletes it once the account is settled.
	initialPasswordFile := auth.InitialPasswordFile(*configPath)

	ensureUser(db, logger, initialPasswordFile)

	manager, err := tunnel.NewManager(db, logger, cipher, cfg.Monitoring.IntervalSec)
	if err != nil {
		log.Fatalf("Failed to create tunnel manager: %v", err)
	}

	// The first reconcile pass runs before anything is served, so the tunnels
	// of the rows that are already stored are up by the time the first request
	// can ask about them.
	logger.Info("Restoring all tunnels...")
	err = manager.RestoreAllTunnels()
	if err != nil {
		logger.Error("failed to restore tunnels", zap.Error(err))
	}

	// From here on the loop is the only thing that starts and stops tunnels. The
	// handlers write rows and wake it up. reconcileDone reports that it returned,
	// because stopping the tunnels while it still runs would let it start them
	// again.
	reconcileCtx, stopReconcile := context.WithCancel(context.Background())
	reconcileDone := make(chan struct{})

	go func() {
		defer close(reconcileDone)
		manager.RunReconcileLoop(reconcileCtx, cfg.Reconcile.IntervalSec)
	}()

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
	g.POST("/setup", authHandler.Setup)

	g.POST("/host", h.CreateHost)
	g.GET("/host", h.ListHosts)
	g.GET("/host/:id", h.GetHost)
	g.PUT("/host/:id", h.UpdateHost)
	g.DELETE("/host/:id", h.DeleteHost)

	g.POST("/service-port", h.CreateServicePort)
	g.GET("/service-port", h.ListServicePorts)
	g.GET("/service-port/:id", h.GetServicePort)
	g.PUT("/service-port/:id", h.UpdateServicePort)
	g.DELETE("/service-port/:id", h.DeleteServicePort)

	g.GET("/status", h.GetStatus)
	g.GET("/status/:hostId", h.GetHostStatus)

	// The UI is put on the instance itself and not on the group above. It is
	// the same bytes for every client and carries no data of its own, while
	// everything it shows comes from /api/**, which stays behind the session.
	web.RegisterRoutes(e)

	// A server that never comes up must not end the process on the spot. The
	// tunnels are restored by now and their rows are in the database, and
	// logger.Fatal would leave both behind.
	serverErr := make(chan error, 1)

	go func() {
		err := e.Start(fmt.Sprintf(":%d", cfg.API.Port))
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	var startErr error

	select {
	case sig := <-sigChan:
		logger.Info("Received signal, shutting down...", zap.String("signal", sig.String()))
	case startErr = <-serverErr:
		logger.Error("failed to start API server", zap.Error(startErr))
	}

	// The API server goes down first so that no request observes tunnels
	// being torn down underneath it. A server that failed to bind has nothing
	// left to serve and answers right away.
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	err = e.Shutdown(ctx)
	if err != nil {
		logger.Error("failed to shut down API server gracefully", zap.Error(err))
	}

	// The loop is stopped before the tunnels are, because it starts again what
	// is stopped while it runs. It returns once the pass it is in ends, and the
	// wait is given up after reconcileStopTimeout: a pass that is still dialing
	// holds nothing that survives this process, which exits as soon as the
	// tunnels are down.
	stopReconcile()

	select {
	case <-reconcileDone:
	case <-time.After(reconcileStopTimeout):
		logger.Warn("the reconcile loop did not return in time, stopping the tunnels anyway",
			zap.Duration("waited", reconcileStopTimeout))
	}

	logger.Info("Stopping all tunnels...")
	manager.StopAllTunnels()
	logger.Info("Exiting tunnel-manager...")

	if startErr != nil {
		// os.Exit does not run the deferred Sync, so the logs are flushed here.
		_ = logger.Sync()
		os.Exit(1)
	}
}
