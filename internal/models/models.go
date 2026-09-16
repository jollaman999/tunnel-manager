package models

import (
	"time"
)

// Host is an SSH endpoint. The gorm default of Enabled applies only when gorm
// inserts the row, so a Host built in Go without reading the database has
// Enabled false and StartTunnel skips it.
type Host struct {
	ID          uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	IP          string    `gorm:"uniqueIndex:idx_hosts_ip;not null" json:"ip"`
	Port        int       `gorm:"not null" json:"port"`
	User        string    `gorm:"not null" json:"user"`
	Password    string    `gorm:"not null" json:"-"`
	Description string    `json:"description"`
	Enabled     bool      `gorm:"default:true" json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ServicePort struct {
	ID          uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	ServiceIP   string    `gorm:"uniqueIndex:idx_service_ip_port;not null" json:"service_ip"`
	ServicePort int       `gorm:"uniqueIndex:idx_service_ip_port;not null" json:"service_port"`
	LocalPort   int       `gorm:"uniqueIndex:idx_service_local_port;not null" json:"local_port"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Tunnel struct {
	HostID          uint      `gorm:"primaryKey;not null" json:"host_id"`
	SPID            uint      `gorm:"primaryKey;not null" json:"sp_id"`
	Status          string    `gorm:"not null" json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `gorm:"default:0" json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
	Server          string    `gorm:"not null" json:"server"`
	Local           string    `gorm:"not null" json:"local"`
	Remote          string    `gorm:"not null" json:"remote"`
}

type CreateHostRequest struct {
	IP          string `json:"ip" validate:"required,ip"`
	Port        int    `json:"port" validate:"required,min=1,max=65535"`
	User        string `json:"user" validate:"required"`
	Password    string `json:"password" validate:"required"`
	Description string `json:"description"`
}

type UpdateHostRequest struct {
	IP          string `json:"ip" validate:"omitempty,ip"`
	Port        *int   `json:"port" validate:"omitempty,min=1,max=65535"`
	User        string `json:"user" validate:"omitempty"`
	Password    string `json:"password" validate:"omitempty"`
	Description string `json:"description"`
	Enabled     *bool  `json:"enabled"`
}

type CreateServicePortRequest struct {
	ServiceIP   string `json:"service_ip" validate:"required,ip"`
	ServicePort int    `json:"service_port" validate:"required,min=1,max=65535"`
	LocalPort   int    `json:"local_port" validate:"required,min=1,max=65535"`
	Description string `json:"description"`
}

type Response struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

// User is the single account the API is served behind. The row is created on
// the first startup with no username and setup_required set, and the username
// and the password are chosen through the API after the first login.
type User struct {
	ID       uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	Username string `gorm:"not null" json:"username"`
	// The hash never leaves the process, so it is kept out of every response
	// the same way the SSH password of a Host is.
	PasswordHash string `gorm:"not null" json:"-"`
	// The column is not given a gorm default. gorm leaves a field at its zero
	// value out of an INSERT when the field carries one, which would write
	// true on the very row that is meant to turn the flag off.
	SetupRequired bool      `gorm:"not null" json:"setup_required"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TableName keeps the table singular. gorm pluralizes User to "users" on its
// own, and the table holds one row. "user" is also the name of an SQL function,
// which the mysql driver keeps apart by quoting every identifier with backticks
// (gorm.io/driver/mysql@v1.5.7/mysql.go:290).
func (User) TableName() string {
	return "user"
}
