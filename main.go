package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"gorm.io/gorm"
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

	"github.com/go-playground/validator/v10"
	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/config"
	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/database"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const version = "1.0.0"

// Maximum time to wait for in-flight HTTP requests to finish on shutdown.
const shutdownTimeout = 10 * time.Second

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

func checkUlimit(logger *zap.Logger) {
	var rLimit syscall.Rlimit
	desiredCur := uint64(65535)

	err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		logger.Warn("error getting rlimit", zap.Error(err))
		return
	}

	logger.Info("current ulimit before change",
		zap.Uint64("cur", rLimit.Cur),
		zap.Uint64("max", rLimit.Max))

	if rLimit.Max < desiredCur {
		logger.Warn("max ulimit is low",
			zap.Uint64("current", rLimit.Max),
			zap.Uint64("desired", desiredCur),
			zap.String("message", "tunnel-manager recommends setting max ulimit to more than 65535 for reliable connection management. raising the max ulimit requires root privileges"))
	}

	if rLimit.Cur >= desiredCur {
		logger.Info("no need to change ulimit")
		return
	}

	// Without root privileges the soft limit can only be raised up to the hard limit.
	newCur := rLimit.Max
	if newCur <= rLimit.Cur {
		logger.Warn("cannot raise the current ulimit any further",
			zap.Uint64("current", rLimit.Cur),
			zap.Uint64("max", rLimit.Max),
			zap.Uint64("desired", desiredCur),
			zap.String("message", "the current ulimit already reached the max ulimit. raising the max ulimit requires root privileges"))
		return
	}

	newLimit := syscall.Rlimit{
		Cur: newCur,
		Max: rLimit.Max,
	}

	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &newLimit)
	if err != nil {
		logger.Warn("failed to change ulimit",
			zap.Error(err),
			zap.Uint64("current", rLimit.Cur),
			zap.Uint64("max", rLimit.Max),
			zap.Uint64("tried", newCur))
		return
	}

	logger.Info("successfully changed ulimit",
		zap.Uint64("old_limit", rLimit.Cur),
		zap.Uint64("new_limit", newLimit.Cur))

	if newLimit.Cur < desiredCur {
		logger.Warn("ulimit is still lower than the desired value",
			zap.Uint64("current", newLimit.Cur),
			zap.Uint64("desired", desiredCur))
	}
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

	if os.Geteuid() != 0 {
		logger.Warn("not running as root",
			zap.Int("euid", os.Geteuid()),
			zap.String("message", "operations that require root privileges may fail, such as raising the max ulimit or writing to system directories"))
	}

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
	logger.Info("loaded the encryption key", zap.String("path", cfg.Security.KeyFile))

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

	manager, err := tunnel.NewManager(db, logger, cipher, cfg.Monitoring.IntervalSec)
	if err != nil {
		log.Fatalf("Failed to create tunnel manager: %v", err)
	}

	logger.Info("Restoring all tunnels...")
	err = manager.RestoreAllTunnels()
	if err != nil {
		logger.Error("failed to restore tunnels", zap.Error(err))
	}

	e := echo.New()
	e.Validator = &CustomValidator{validator: validator.New()}
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	h := api.NewHandler(db, manager, logger, cipher)
	g := e.Group("/api")

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

	logger.Info("Stopping all tunnels...")
	manager.StopAllTunnels()
	logger.Info("Exiting tunnel-manager...")

	if startErr != nil {
		// os.Exit does not run the deferred Sync, so the logs are flushed here.
		_ = logger.Sync()
		os.Exit(1)
	}
}
