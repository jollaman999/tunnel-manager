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
	PrivateKey    string `json:"-"`
	KeyPassphrase string `json:"-"`
	// HostKey is the public key the SSH server of this Host is trusted on,
	// written as "<algorithm> <base64>", which is an authorized_keys line
	// without the comment that may follow it. A connection is made only to a
	// server that presents exactly this key, so a Host that carries none
	// reaches nothing until the key that was presented has been approved.
	//
	// PendingHostKey is what a server presented on a connection that was
	// refused for that reason, in the same form. It is what the approval is
	// asked about, and it is never promoted to HostKey by anything but a
	// person: nothing this end can see makes a key the right one.
	//
	// Neither is kept out of responses the way the password and the private
	// key are, because a public key is not a secret. What is put on a screen
	// is the SHA256 fingerprint rather than the key itself, which is short
	// enough to read out and compare against what the server says about
	// itself, so an answer carrying one of these turns it into a fingerprint
	// first.
	//
	// Neither carries "not null". The columns are added to installations
	// whose rows were written before they existed, and AutoMigrate fills
	// those with NULL, the way it did for Tunnel.ServerBanner below.
	HostKey        string    `json:"host_key"`
	PendingHostKey string    `json:"pending_host_key"`
	Description    string    `json:"description"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
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

// BindScopeLoopback and BindScopeWildcard are the two answers an assignment
// may carry about where its forwarded port is opened. Each names a pair of
// addresses rather than one, because the two address families do not stand in
// for each other: a port opened on 127.0.0.1 is not reached by a client that
// connects to ::1, and the same holds for the two wildcards. Whoever chose a
// scope chose it for the machine and not for one family of addresses, so both
// are asked for and whichever the far side refuses is reported afterwards.
//
// They are stored as these words rather than as the addresses themselves so
// that the stored row says what was chosen. An address would have to be read
// back into one of these two anyway, and a row holding an address that is
// neither, which is what a hand-written value would be, has no answer.
const (
	BindScopeLoopback = "loopback"
	BindScopeWildcard = "wildcard"
)

// HostServicePort is one assignment: this Host is to carry this service port.
// The pair is the whole of the row, so the two columns are the primary key
// together, the way they are on Tunnel below, and the database itself refuses
// to hold the same assignment twice.
//
// The row says nothing about whether the Host is enabled. Assigning a service
// port and running a tunnel for it are different questions, and the second one
// is answered where the tunnels are reconciled: a Host that is disabled keeps
// its assignments so that enabling it again brings its tunnels back rather than
// leaving it with none.
type HostServicePort struct {
	HostID uint `gorm:"primaryKey;not null" json:"host_id"`
	SPID   uint `gorm:"primaryKey;not null" json:"sp_id"`
	// BindScope is where on the Host the forwarded port of this assignment is
	// asked to be opened: BindScopeLoopback asks for the loopback addresses
	// and BindScopeWildcard for the wildcards.
	//
	// It is called a scope rather than a reach because Tunnel.ForwardReach is
	// already the other half of the question. That one is what a connection
	// found afterwards, measured; this one is what was asked for before
	// anything was measured, and the two want names that are not read as one.
	//
	// It sits on the assignment because the Host and the service port both
	// have a say in it. One Host carries several service ports, and one of
	// those may be meant for that machine alone while the next is to be
	// reached from elsewhere; one service port is carried by several Hosts,
	// and only some of them face a network nobody else should reach in over.
	// Either end alone forces one answer on the other, and the pair is the
	// smallest row that can hold both.
	//
	// An empty value means BindScopeWildcard, which is what every row written
	// before the column existed holds: AutoMigrate adds the column and leaves
	// what is stored alone. Reading the empty value as the loopback instead
	// would take reach away from every tunnel that is running, on a startup
	// that was asked for nothing of the sort.
	//
	// The rule is a constraint on the column so that the database refuses a
	// value that is neither, the way it refuses the same assignment twice.
	// Assignments are written as model values from several places rather than
	// bound from one request, so a rule carried by a request struct would
	// leave most of those writers outside it. The two words are spelled again
	// in the tag because a struct tag holds text and not an expression, and
	// the empty value is among them because that is the wildcard.
	BindScope string `gorm:"check:chk_host_service_ports_bind_scope,bind_scope IN ('','loopback','wildcard')" json:"bind_scope"`
	// CreatedAt records when the assignment was made. There is no UpdatedAt
	// beside it because an assignment has nothing to change: both of its
	// columns are the key, so it is written or it is removed.
	CreatedAt time.Time `json:"created_at"`
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
	// ServerBanner is what the SSH server called itself on the handshake, as
	// the library hands it over (x/crypto/ssh, sshConn.ServerVersion). It is
	// kept because what opens a forwarded port to an address other than
	// loopback differs by server: OpenSSH decides it with GatewayPorts in
	// sshd_config, Dropbear with the -a flag on its command line. The value is
	// a string the far side chose, so whatever draws it treats it as text and
	// never as markup.
	//
	// It carries no "not null". The column is added to installations whose
	// rows were written before it existed, and AutoMigrate fills those with
	// NULL.
	ServerBanner string `json:"server_banner"`
	// ForwardReach is whether the forwarded port answered a TCP connection
	// from this process, measured once per connection: "reachable",
	// "unreachable", or "unknown" while nothing has been measured. Anything
	// else, an empty value on a row from before the column among them, means
	// the same as "unknown".
	//
	// It says where the port was not reached from and never why. A server that
	// bound the port to loopback alone and a firewall on the way look exactly
	// the same to a connection that does not arrive, so the two cannot be told
	// apart from here and neither may be reported as the cause.
	ForwardReach string `json:"forward_reach"`
	// ErrorKind names what sort of failure LastError is, for the one sort the
	// screen has something to say about: "forward_denied" is the SSH server
	// refusing to open the forwarded port, and an empty value is every other
	// failure and every row that is not in error.
	//
	// It is here so that the screen decides on a name this program chose
	// rather than on the sentence the SSH library wrote. That sentence is a
	// plain errors.New with nothing exported to compare against, so it is
	// matched once, in the one place that makes the call, and what travels is
	// this.
	//
	// Like ForwardReach it says what happened and not why. The server refuses
	// without giving a reason, and the several settings that make it refuse
	// look identical from here, so none of them may be reported as the cause.
	ErrorKind string `json:"error_kind"`
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
	// AssignAllServicePorts is whether the Host is to carry every service port
	// that is stored when it is registered. A Host with no assignment runs no
	// tunnel at all, and carrying everything is what an installation did before
	// the assignments were rows of their own, so a request that does not
	// mention the field is answered that way.
	//
	// It is a pointer for the reason Enabled is: a request asking for a Host
	// with no assignments has to be told apart from one that says nothing, and
	// on a plain bool the two arrive the same.
	AssignAllServicePorts *bool `json:"assign_all_service_ports"`
	// BindScope is what every assignment this registration makes is opened to.
	// A Host is registered before anything has been said about its service
	// ports one at a time, so the one answer given here is what the whole batch
	// starts on, and each assignment is moved on its own afterwards on the
	// assignment screen of the Host.
	//
	// It is read only where those assignments are written, which is when
	// AssignAllServicePorts asks for them. The Host itself holds no scope:
	// there is nowhere else for this to be kept.
	//
	// An empty value is the wildcard, which is what an empty column means on
	// HostServicePort above, so the rule takes a request that leaves the field
	// out and refuses a word that is neither of the two.
	BindScope string `json:"bind_scope" validate:"omitempty,oneof=loopback wildcard"`
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
	// AssignToAllHosts is whether every stored Host is to carry this service
	// port from the moment it is registered. It is a pointer, and a request
	// that leaves it out asks for the assignments, for the reasons given on
	// CreateHostRequest.AssignAllServicePorts above.
	AssignToAllHosts *bool `json:"assign_to_all_hosts"`
	// BindScope is what the assignments AssignToAllHosts makes are opened to,
	// the batch of them, for the reason CreateHostRequest.BindScope carries
	// one.
	//
	// One answer can stand for a batch that reaches every Host because the two
	// words mean the same thing on every machine. An address typed in by hand
	// would not: it would name an interface of one of them, and the rest would
	// be asked to open a port on an address they do not have.
	BindScope string `json:"bind_scope" validate:"omitempty,oneof=loopback wildcard"`
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
