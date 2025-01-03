package main

import (
	"flag"
	"fmt"
	"gorm.io/gorm"
	"log"
	"strconv"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/api"
	"github.com/jollaman999/tunnel-manager/internal/config"
	"github.com/jollaman999/tunnel-manager/internal/database"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
)

func initDatabase(cfg *config.Config, logger *zap.Logger) (*gorm.DB, error) {
	timeout := time.After(time.Duration(cfg.Database.TimeoutSec) * time.Second)
	tick := time.Tick(1 * time.Second)

	for {
		select {
		case <-timeout:
			return nil, fmt.Errorf("timeout waiting for database connection after " +
				strconv.Itoa(cfg.Database.TimeoutSec) + " seconds")
		case <-tick:
			db, err := database.NewDatabase(
				cfg.Database.Host,
				cfg.Database.Port,
				cfg.Database.User,
				cfg.Database.Password,
				cfg.Database.Name,
			)
			if err != nil {
				logger.Info("attempting to connect to database...",
					zap.String("host", cfg.Database.Host),
					zap.Int("port", cfg.Database.Port))
				continue
			}
			logger.Info("successfully connected to database")
			return db, nil
		}
	}
}

func main() {
	// Initialize Logger
	logger, _ := zap.NewProduction()
	defer func() {
		_ = logger.Sync()
	}()

	configPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// Load configuration
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize database
	db, err := initDatabase(cfg, logger)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	// Initialize tunnel manager
	manager, err := tunnel.NewManager(db, logger, cfg.Monitoring.IntervalSec)
	if err != nil {
		log.Fatalf("Failed to create tunnel manager: %v", err)
	}

	// Initialize Echo
	e := echo.New()

	// Middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	// Initialize handlers
	h := api.NewHandler(db, manager, logger)

	// Routes
	group := e.Group("/api")

	// VM routes
	group.POST("/vms", h.CreateVM)
	group.GET("/vms", h.ListVMs)
	group.GET("/vms/:id", h.GetVM)
	group.PUT("/vms/:id", h.UpdateVM)
	group.DELETE("/vms/:id", h.DeleteVM)

	// Service Port routes
	group.POST("/service-ports", h.CreateServicePort)
	group.GET("/service-ports", h.ListServicePorts)
	group.GET("/service-ports/:id", h.GetServicePort)
	group.PUT("/service-ports/:id", h.UpdateServicePort)
	group.DELETE("/service-ports/:id", h.DeleteServicePort)

	// Status routes
	group.GET("/status", h.GetStatus)
	group.GET("/status/:vmId", h.GetVMStatus)

	if err := manager.RestoreAllTunnels(); err != nil {
		logger.Error("failed to restore tunnels", zap.Error(err))
	}

	// Start server
	e.Logger.Fatal(e.Start(fmt.Sprintf(":%d", cfg.API.Port)))
}
