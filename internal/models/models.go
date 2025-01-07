package models

import (
	"gorm.io/gorm"
	"time"
)

type VM struct {
	gorm.Model
	IP          string `gorm:"index:idx_vms_ip,unique,where:deleted_at IS NULL" json:"ip"`
	Port        int    `gorm:"not null" json:"port"`
	User        string `gorm:"not null" json:"user"`
	Password    string `gorm:"not null" json:"-"`
	Description string `json:"description"`
}

type ServicePort struct {
	ID          uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	ServiceIP   string `gorm:"not null" json:"service_ip"`
	ServicePort int    `gorm:"not null" json:"service_port"`
	LocalPort   int    `gorm:"not null" json:"local_port"`
	Description string `json:"description"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	DeletedAt   gorm.DeletedAt `gorm:"index"`

	_ int `gorm:"uniqueIndex:idx_service_ip_port,priority:1,cols:service_ip,service_port,deleted_at;where:deleted_at IS NULL"`
}

type Tunnel struct {
	VMID            uint      `gorm:"primaryKey;not null" json:"vm_id"`
	SPID            uint      `gorm:"primaryKey;not null" json:"sp_id"`
	Status          string    `gorm:"not null" json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `gorm:"default:0" json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
	Server          string    `gorm:"not null" json:"server"`
	Local           string    `gorm:"not null" json:"local"`
	Remote          string    `gorm:"not null" json:"remote"`
}

type CreateVMRequest struct {
	IP          string `json:"ip" validate:"required,ip"`
	Port        int    `json:"port" validate:"required,min=1,max=65535"`
	User        string `json:"user" validate:"required"`
	Password    string `json:"password" validate:"required"`
	Description string `json:"description"`
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
