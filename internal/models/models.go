package models

import (
	"time"
)

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
	LocalPort   int       `gorm:"not null" json:"local_port"`
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
