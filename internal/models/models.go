package models

import (
	"time"
)

// Host is an SSH endpoint.
//
// Enabled carries no database default. It used to have one, and a default on a
// bool is a trap: gorm leaves a field out of an insert when it holds the zero
// value and the column has a default, so a Host inserted as disabled came back
// enabled and started connecting. Naming the column in Select does not change
// it. Whatever inserts a Host says what Enabled is, and what an absent field
// means is decided where absence can be told from false.
type Host struct {
	ID   uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	IP   string `gorm:"uniqueIndex:idx_hosts_ip;not null" json:"ip"`
	Port int    `gorm:"not null" json:"port"`
	User string `gorm:"not null" json:"user"`
	// Password carries no "not null" because a Host may be registered with a
	// private key and no password at all. It used to be required, from when a
	// password was the only way in, and a Host that has only a key would have
	// had to be given an empty string to satisfy a column that says a password
	// is always there.
	Password string `json:"-"`
	// PrivateKey is the PEM private key this Host is authenticated with, sealed
	// with crypto.Cipher the way Password is, and KeyPassphrase is what opens
	// it when the key is protected by one. Both are kept out of every response:
	// a key that leaves this process is a key into every machine that trusts
	// it, and a passphrase beside it takes the protection off.
	PrivateKey    string    `json:"-"`
	KeyPassphrase string    `json:"-"`
	Description   string    `json:"description"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
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

// CreateHostRequest registers a Host. The password is no longer required on its
// own: a Host is registered with a private key, with a password, or with both,
// and which of them is missing is decided in the handler rather than by a rule
// on one field, so that the refusal can say what to do about it.
type CreateHostRequest struct {
	IP            string `json:"ip" validate:"required,ip"`
	Port          int    `json:"port" validate:"required,min=1,max=65535"`
	User          string `json:"user" validate:"required"`
	Password      string `json:"password" validate:"omitempty"`
	PrivateKey    string `json:"private_key" validate:"omitempty"`
	KeyPassphrase string `json:"key_passphrase" validate:"omitempty"`
	Description   string `json:"description"`
	// Enabled is a pointer so that a Host asked for as disabled can be told
	// from one that did not mention it. A plain bool cannot say the difference,
	// and the two mean different things: the second one is enabled.
	Enabled *bool `json:"enabled"`
}

// UpdateHostRequest changes a Host. A field the request leaves out is left as
// it is, the private key and its passphrase included: an empty box on the
// screen keeps the key that is stored rather than taking it away.
type UpdateHostRequest struct {
	IP            string `json:"ip" validate:"omitempty,ip"`
	Port          *int   `json:"port" validate:"omitempty,min=1,max=65535"`
	User          string `json:"user" validate:"omitempty"`
	Password      string `json:"password" validate:"omitempty"`
	PrivateKey    string `json:"private_key" validate:"omitempty"`
	KeyPassphrase string `json:"key_passphrase" validate:"omitempty"`
	Description   string `json:"description"`
	Enabled       *bool  `json:"enabled"`
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
// which the driver keeps apart by quoting every identifier with backticks
// (github.com/glebarez/sqlite@v1.11.0/sqlite.go:146, QuoteTo).
func (User) TableName() string {
	return "user"
}
