package main

import (
	"flag"
	"fmt"
	"gorm.io/gorm"
	"log"
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
	"github.com/jollaman999/tunnel-manager/internal/database"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

const version = "0.0.3"

func initDatabase(cfg *config.Config, logger *zap.Logger) (*gorm.DB, error) {
	timeout := time.After(time.Duration(cfg.Database.TimeoutSec) * time.Second)
	tick := time.Tick(1 * time.Second)

	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for database connection after %s seconds", strconv.Itoa(cfg.Database.TimeoutSec))
		case <-tick:
			db, err := database.NewDatabase(cfg.Database.Host, cfg.Database.Port, cfg.Database.User, cfg.Database.Password, cfg.Database.Name)
			if err != nil {
				logger.Info("attempting to connect to database...", zap.String("host", cfg.Database.Host), zap.Int("port", cfg.Database.Port))
				continue
			}
			logger.Info("successfully connected to database")
			return db, nil
		}
	}
}

func initLogger(cfg *config.Config) (*zap.Logger, error) {
	logDir := filepath.Dir(cfg.Logging.File.Path)
	err := os.MkdirAll(logDir, 0755)
	if err != nil {
		return nil, fmt.Errorf("failed to create log directory: %v", err)
	}

	logFile := cfg.Logging.File.Path
	_, err = os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %v", err)
	}

	logWriter := &lumberjack.Logger{
		Filename:   cfg.Logging.File.Path,
		MaxSize:    cfg.Logging.File.MaxSize,
		MaxBackups: cfg.Logging.File.MaxBackups,
		MaxAge:     cfg.Logging.File.MaxAge,
		Compress:   cfg.Logging.File.Compress,
	}

	var level zapcore.Level
	err = level.UnmarshalText([]byte(cfg.Logging.Level))
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

	core := zapcore.NewTee(
		zapcore.NewCore(
			encoder,
			zapcore.AddSync(logWriter),
			level,
		),
		zapcore.NewCore(
			encoder,
			zapcore.AddSync(os.Stdout),
			level,
		))

	return zap.New(core, zap.AddCaller()), nil
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
			zap.String("message", "tunnel-manager recommends setting max ulimit to more than 65535 for reliable connection management"))
	}

	if rLimit.Max >= desiredCur && rLimit.Cur >= desiredCur {
		logger.Info("no need to change ulimit")
		return
	}

	if rLimit.Max > desiredCur {
		desiredCur = rLimit.Max
	}

	newLimit := syscall.Rlimit{
		Cur: desiredCur,
		Max: rLimit.Max,
	}

	err = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &newLimit)
	if err != nil {
		logger.Warn("failed to change ulimit",
			zap.Error(err),
			zap.Uint64("current", rLimit.Cur),
			zap.Uint64("max", rLimit.Max))
		return
	}

	logger.Info("successfully changed ulimit",
		zap.Uint64("old_limit", rLimit.Cur),
		zap.Uint64("new_limit", newLimit.Cur))
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

	if os.Geteuid() != 0 {
		log.Fatal("This program must be run as root")
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

	checkUlimit(logger)

	db, err := initDatabase(cfg, logger)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	manager, err := tunnel.NewManager(db, logger, cfg.Monitoring.IntervalSec)
	if err != nil {
		log.Fatalf("Failed to create tunnel manager: %v", err)
	}

	logger.Info("Restoring all tunnels...")
	err = manager.RestoreAllTunnels()
	if err != nil {
		logger.Error("failed to restore tunnels", zap.Error(err))
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		logger.Info("Stopping all tunnels...")
		manager.StopAllTunnels()
		logger.Info("Exiting tunnel-manager...")
		os.Exit(0)
	}()

	e := echo.New()
	e.Validator = &CustomValidator{validator: validator.New()}
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	h := api.NewHandler(db, manager, logger)
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

	e.Logger.Fatal(e.Start(fmt.Sprintf(":%d", cfg.API.Port)))
}
