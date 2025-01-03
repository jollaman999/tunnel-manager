package models

import (
	"gorm.io/gorm"
	"time"
)

type VM struct {
	gorm.Model
	IP          string   `gorm:"uniqueIndex;not null" json:"ip"`
	Port        int      `gorm:"not null" json:"port"`
	User        string   `gorm:"not null" json:"user"`
	Password    string   `gorm:"not null" json:"password"`
	Description string   `json:"description"`
	Tunnels     []Tunnel `gorm:"foreignKey:VMID" json:"tunnels,omitempty"`
}

type ServicePort struct {
	gorm.Model
	ServiceIP   string `gorm:"not null" json:"service_ip"`
	ServicePort int    `gorm:"not null" json:"service_port"`
	LocalPort   int    `gorm:"not null" json:"local_port"`
	Description string `json:"description"`
}

type Tunnel struct {
	gorm.Model
	VMID            uint      `gorm:"not null" json:"vm_id"`
	Status          string    `gorm:"not null" json:"status"`
	LastError       string    `json:"last_error"`
	RetryCount      int       `gorm:"default:0" json:"retry_count"`
	LastConnectedAt time.Time `json:"last_connected_at"`
	VM              *VM       `gorm:"foreignKey:VMID" json:"-"`
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
