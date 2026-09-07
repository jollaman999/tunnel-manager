package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Queries slower than this are reported without logging every statement, though
// only up to the "warn" level. "error" and above report failed queries alone.
const slowQueryThreshold = 200 * time.Millisecond

// gormLogLevel maps an application log level to the level gorm understands.
// gorm only logs every statement at Info, which is far too noisy for the
// default setup, so plain SQL tracing is kept for "debug" only. "info" and
// "warn" report failed and slow queries, while "error" and above report
// failed queries alone.
func gormLogLevel(level string) gormlogger.LogLevel {
	switch level {
	case "debug":
		return gormlogger.Info
	case "info", "warn":
		return gormlogger.Warn
	default:
		return gormlogger.Error
	}
}

// zapGormLogger sends gorm output through zap so that the format, the log file
// and the level are the same as for the rest of the application.
type zapGormLogger struct {
	logger *zap.Logger
	level  gormlogger.LogLevel
}

func (l *zapGormLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	newLogger := *l
	newLogger.level = level
	return &newLogger
}

func (l *zapGormLogger) Info(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Info {
		l.logger.Info(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Warn(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Warn {
		l.logger.Warn(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Error(_ context.Context, msg string, data ...interface{}) {
	if l.level >= gormlogger.Error {
		l.logger.Error(fmt.Sprintf(msg, data...))
	}
}

func (l *zapGormLogger) Trace(_ context.Context, begin time.Time, fc func() (string, int64), err error) {
	if l.level <= gormlogger.Silent {
		return
	}

	elapsed := time.Since(begin)
	fields := func() []zap.Field {
		sql, rows := fc()
		return []zap.Field{
			zap.String("sql", sql),
			zap.Int64("rows", rows),
			zap.Float64("elapsed_ms", float64(elapsed.Nanoseconds())/1e6),
		}
	}

	switch {
	case err != nil && l.level >= gormlogger.Error && !errors.Is(err, gormlogger.ErrRecordNotFound):
		l.logger.Error("query failed", append(fields(), zap.Error(err))...)
	case elapsed > slowQueryThreshold && l.level >= gormlogger.Warn:
		l.logger.Warn("slow query", append(fields(),
			zap.Float64("slow_threshold_ms", float64(slowQueryThreshold.Nanoseconds())/1e6))...)
	case l.level >= gormlogger.Info:
		l.logger.Debug("query", fields()...)
	}
}

func NewDatabase(host string, port int, user, password, dbname string, logger *zap.Logger, logLevel string) (*gorm.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		user, password, host, port, dbname)

	config := &gorm.Config{
		Logger: &zapGormLogger{
			logger: logger.Named("gorm"),
			level:  gormLogLevel(logLevel),
		},
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	}

	db, err := gorm.Open(mysql.Open(dsn), config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	err = db.AutoMigrate(
		&models.Host{},
		&models.ServicePort{},
		&models.Tunnel{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	return db, nil
}
