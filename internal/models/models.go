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
//
// Address and Port are unique together rather than Address alone. The same
// private address is a different machine behind each Host it is reached
// through, and a rule on the address alone refused the second of them. The
// index keeps the name it had when it covered the address alone, which is what
// database.widenHostAddressIndex moves an installation over by.
type Host struct {
	ID      uint   `gorm:"primaryKey;autoIncrement" json:"id"`
	Address string `gorm:"uniqueIndex:idx_hosts_address;not null" json:"address"`
	Port    int    `gorm:"uniqueIndex:idx_hosts_address;not null" json:"port"`
	User    string `gorm:"not null" json:"user"`
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
	HostKey        string `json:"host_key"`
	PendingHostKey string `json:"pending_host_key"`
	Description    string `json:"description"`
	Enabled        bool   `json:"enabled"`
	// SocksEnabled, SocksPort, SocksBindScope and SocksAllowedSources are the
	// SOCKS5 proxy this Host carries: a port opened on this machine whose
	// clients name a target on every connection, and the Host dials it. It
	// runs while the Host is enabled, SocksEnabled is set and SocksPort is
	// above zero.
	//
	// None of them carries "not null" or a default. The columns are added to
	// installations whose rows were written before they existed, and
	// AutoMigrate fills those with NULL, which reads back as the zero value:
	// no proxy. SocksEnabled has no default for the reason Enabled has none.
	//
	// SocksBindScope takes the words and the constraint of
	// HostServicePort.BindScope. What an empty value means is read by
	// SocksScope, since it depends on whether the proxy runs.
	//
	// SocksAllowedSources is the list of addresses and CIDR blocks a client of
	// the proxy may connect from, separated by commas or spaces. An empty
	// value lets every address in. The proxy asks for no password, so on the
	// wildcard this list is what keeps it from being open to anyone who can
	// reach the port.
	SocksEnabled        bool      `json:"socks_enabled"`
	SocksPort           int       `json:"socks_port"`
	SocksBindScope      string    `gorm:"check:chk_hosts_socks_bind_scope,socks_bind_scope IN ('','loopback','wildcard')" json:"socks_bind_scope"`
	SocksAllowedSources string    `json:"socks_allowed_sources"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// SocksRuns reports whether the SOCKS5 proxy of the Host is switched on with a
// port, which is what opens it while the Host is enabled. A Host that is
// disabled is counted with the ones that run, since enabling it opens the
// proxy again as it was.
func (h Host) SocksRuns() bool {
	return h.SocksEnabled && h.SocksPort > 0
}

// SocksScope is the scope the SOCKS5 proxy of the Host is opened on, as one of
// the two words. A stored word is itself.
//
// The empty value is what every row written before the column existed holds,
// and what an import of a file that named no scope stored. A proxy that runs
// with it has been running on the wildcard, and it is read that way so that an
// upgrade does not take the proxy away from the machines that use it. A proxy
// that does not run opens nothing, and the first time it does it is on
// loopback: it asks for no password, so the wildcard is to be chosen by its
// word rather than handed over by a column nobody filled in.
func (h Host) SocksScope() string {
	if h.SocksBindScope != "" {
		return h.SocksBindScope
	}

	if h.SocksRuns() {
		return BindScopeWildcard
	}

	return BindScopeLoopback
}

// HostJump is one step of the jump route of a Host: the connection to HostID
// passes through JumpHostID on its way, as the Seq-th SSH server of the route,
// counted from 1 at the one this machine connects to directly. A Host with no
// row here is connected to directly.
//
// The route is a list per Host rather than a single previous step on each
// Host. With one previous step, a Host put into the route of another took
// that route on as its own, and changing the route of one Host changed the
// way to every Host that passes through it. The route is also taken as it is
// written: the route of a Host that is passed through is not followed, so
// what a row says is the whole of the way.
//
// The Host and the step are the key together, the way the Host and the number
// are on LocalForward, so a route cannot hold two different Hosts at one step.
type HostJump struct {
	HostID     uint `gorm:"primaryKey;not null" json:"host_id"`
	Seq        uint `gorm:"primaryKey;not null" json:"seq"`
	JumpHostID uint `gorm:"not null" json:"jump_host_id"`
}

type ServicePort struct {
	ID             uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	ServiceAddress string    `gorm:"uniqueIndex:idx_service_address_port;not null" json:"service_address"`
	ServicePort    int       `gorm:"uniqueIndex:idx_service_address_port;not null" json:"service_port"`
	LocalPort      int       `gorm:"uniqueIndex:idx_service_local_port;not null" json:"local_port"`
	Description    string    `json:"description"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
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
	// It is called a scope rather than a reach because a reach is what a
	// connection finds afterwards, and this is what was asked for before
	// anything was tried. The two want names that are not read as one.
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
	// Enabled is whether the tunnel of this assignment runs. One that is off
	// stays assigned, with its scope, and runs no tunnel, so a service port can
	// be paused on one Host without being taken away and put back.
	//
	// It carries no database default for the reason Host.Enabled carries none:
	// an assignment written as off would be stored as on. Every writer says
	// what it is. It carries no "not null" either, because the column is added
	// to installations whose rows were written before it existed, and
	// AutoMigrate fills those with NULL. What NULL is taken to mean is at
	// database.fillAssignmentsEnabled.
	Enabled bool `json:"enabled"`
	// CreatedAt records when the assignment was made. There is no UpdatedAt
	// beside it because an assignment has little to change: both of its key
	// columns are the pair, and the scope and whether it runs are moved in
	// place.
	CreatedAt time.Time `json:"created_at"`
}

// LocalForward is one local forward carried by a Host: this machine opens
// LocalPort and every connection to it is carried over the SSH connection of
// the Host to TargetAddress:TargetPort, as seen from the Host. It runs the other way
// from a ServicePort, so it is a table of its own rather than a direction on
// that one, where the local port and the service address would mean different
// things from row to row.
//
// LocalPort is unique over the table because every row opens its port on this
// same machine, and two rows asking for one port leave one of them unable to
// start.
//
// A row is identified by its Host and its Number together. The number is what
// the screen draws, what a log line carries and what a path names, and it is
// the key rather than a column beside a table-wide identifier so that there is
// only one of it: a row read off the screen as the second forward of Host 3 is
// the row a log line calls the second forward of Host 3.
type LocalForward struct {
	HostID uint `gorm:"primaryKey;not null" json:"host_id"`
	// Number is which forward of its Host this is, counted from 1. Numbers are
	// handed out per Host, so two Hosts each have a first forward.
	Number uint `gorm:"primaryKey;not null" json:"number"`
	// BindScope is where on the Tunnel Manager server LocalPort is opened,
	// with the words and the constraint HostServicePort.BindScope carries, and
	// an empty value is the wildcard for the same reason.
	BindScope     string `gorm:"check:chk_local_forwards_bind_scope,bind_scope IN ('','loopback','wildcard')" json:"bind_scope"`
	LocalPort     int    `gorm:"uniqueIndex:idx_local_forwards_local_port;not null" json:"local_port"`
	TargetAddress string `gorm:"not null" json:"target_address"`
	TargetPort    int    `gorm:"not null" json:"target_port"`
	Description   string `json:"description"`
	// AllowedSources is the list of addresses and CIDR blocks a client of the
	// local port may connect from, in the form Host.SocksAllowedSources takes.
	// An empty value lets every address in, which is what every forward did
	// before the column existed, so it carries no "not null" and no default
	// for the reason the SOCKS5 columns carry none. It is held to the list on
	// either scope, the way the proxy is: on the wildcard it is what keeps the
	// port from being open to anyone who can reach it.
	AllowedSources string `json:"allowed_sources"`
	// Enabled is whether this forward runs. One that is off opens no port and
	// makes no SSH connection, and still holds LocalPort. It carries no
	// database default for the reason Host.Enabled carries none.
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Tunnel struct {
	// HostID is the Host the tunnel runs over, whose SSH connection opens the
	// forwarded port.
	HostID uint `gorm:"primaryKey;not null" json:"host_id"`
	// SPID is the service port the tunnel carries. A Host runs one tunnel for
	// each service port assigned to it, so the two ids together name a row.
	SPID uint `gorm:"primaryKey;not null" json:"sp_id"`
	// Status is where the connection stands, as one of these words:
	//   "starting"            - built and not connected yet
	//   "connected"           - the SSH connection stands and the port is open
	//   "reconnecting"        - the connection dropped, or the wait after a
	//                           failure is over, and the next attempt is on
	//   "error"               - the last attempt to connect or to open the
	//                           port failed, and LastError says how
	//   "host_key_unapproved" - the Host presented a key nobody has approved
	//   "host_key_mismatch"   - the Host presented a key other than the one
	//                           approved
	//   "jump_host_disabled"  - the Host is reached through a Host that is
	//                           disabled
	//
	// Most errors are tried again after a wait, which is when the row moves to
	// reconnecting. A refused host key, refused credentials and a route that
	// is not to be dialled are not: no number of attempts changes them, and
	// what does change them, an approval or a change to a Host, builds the
	// tunnel again.
	//
	// A local forward row of the status table carries the same words, and
	// three of its own for a forward that runs nothing: "disabled" when its
	// Host is disabled, "off" when the forward is switched off, and "stopped"
	// when both are on and no forward is running.
	Status string `gorm:"not null" json:"status"`
	// LastError is what the last failure said, as the SSH library or this
	// program wrote it. It is emptied when a connection stands, and kept while
	// the row is reconnecting so that what brought it down can still be read.
	// It is a sentence for a person; ErrorKind and JumpReason are the values
	// to branch on.
	LastError string `json:"last_error"`
	// RetryCount is how many times the tunnel has gone to reconnecting since
	// a connection last stood, and goes back to 0 when one does. A number that
	// keeps climbing is a tunnel that does not come up.
	RetryCount int `gorm:"default:0" json:"retry_count"`
	// LastConnectedAt is when a connection last stood. It is kept when the
	// connection goes down, so on a row that is reconnecting it is when the
	// tunnel was last up, and it is the zero time, 0001-01-01T00:00:00Z, on a
	// row that has not connected since it was started.
	LastConnectedAt time.Time `json:"last_connected_at"`
	// Server is the SSH address of the Host, address and port. On a Host
	// reached through jump hosts it is still the Host itself and not the
	// first step of the route.
	Server string `gorm:"not null" json:"server"`
	// Local is the address the forwarded port is opened on, on the Host. The
	// bind scope names two, an IPv4 one and an IPv6 one, and the row carries
	// one of them, the one to connect to: the IPv4 one, unless a connection
	// opened the IPv6 one alone, because the row has to name an address that
	// answers. Until a connection stands it is the IPv4 one that was asked
	// for. Which halves opened is on OpenReach, and what the Host says is
	// listening is on ListenAddresses.
	//
	// On a local forward row of the status table it is the address opened on
	// the Tunnel Manager server instead.
	Local string `gorm:"not null" json:"local"`
	// Remote is the service the forwarded port carries to, address and port,
	// as reached from the Tunnel Manager server. On a local forward row of the
	// status table it is the target reached from the Host instead.
	Remote string `gorm:"not null" json:"remote"`
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
	// It says what happened and not why. The server refuses
	// without giving a reason, and the several settings that make it refuse
	// look identical from here, so none of them may be reported as the cause.
	ErrorKind string `json:"error_kind"`
	// JumpSeq, JumpHostID and JumpReason say where on the jump route of the
	// Host the connection stopped and why, for a failure that is about a Host
	// passed through rather than the Host itself: the step counted from 1 the
	// way HostJump.Seq is, the Host at that step, and one of "disabled",
	// "dial", "auth", "host_key" and "route" (tunnel.JumpReasonDisabled and
	// the rest). All three are empty on a row that is not in error and on a
	// failure that is not about the route, and a "route" refusal leaves the
	// step and the Host empty when it is about the whole of the route, one
	// longer than it may be.
	//
	// They are here for the reason ErrorKind is: LastError names the step in a
	// sentence, and the screen says what to do about it in its own words.
	//
	// None of them carries "not null", for the reason ServerBanner does not.
	JumpSeq int `json:"jump_seq"`
	// JumpHostID is the Host at step JumpSeq of the jump route, the one the
	// connection stopped at, and 0 where JumpSeq is.
	JumpHostID uint `json:"jump_host_id"`
	// JumpReason is why the connection stopped on the jump route, one of the
	// words named on JumpSeq, and empty on a failure that is not about the
	// route.
	JumpReason string `json:"jump_reason"`
	// OpenReach says which of the two addresses of the bind scope went up.
	//   "both"  - both went up
	//   "ipv4"  - only 127.0.0.1 or 0.0.0.0 went up; the IPv6 one did not
	//   "ipv6"  - only ::1 or :: went up; the IPv4 one did not
	//   ""      - nothing measured yet, or a row written before this column
	//
	// Who says so differs by the sort of row, and so does what the reading is
	// worth.
	//
	// On a service port row it is what the SSH server answered. The port is
	// opened on the Host, which is asked once for each address and answers
	// each request on its own, so the assignment can end up half open. It says
	// what was answered and not why: a server that refuses the second request
	// because it already bound both families on the first and one that has no
	// IPv6 at all answer identically, so neither may be reported as the cause,
	// and an answer is not a binding.
	//
	// On a local forward row it is what the Tunnel Manager server itself
	// opened, which is a measurement and not an answer. The port is opened on
	// this machine, so a half that is missing is one it could not open: another
	// program holds the port on that address, or the address is not available
	// here. Local then names the half that opened. The SOCKS5 proxy of a Host
	// opens its port the same way and reports the same reading as
	// socks_open_reach on the Host.
	//
	// The empty value is nothing measured rather than nothing open: a row that
	// has not been connected yet and a row of which one half did not go up must
	// not read alike, and a screen that draws a warning on the first would put
	// one on every tunnel that is still starting.
	//
	// Neither half going up is not one of these readings. Nothing is forwarded
	// then, which is a failure of the forward rather than a fact about how far
	// it reaches, and it is reported the way every other failure is, on Status,
	// LastError and, on a service port row, ErrorKind.
	OpenReach string `json:"open_reach"`
	// ListenAddresses are the addresses the Host itself said are listening on
	// the forwarded port, comma separated, each one an address that parsed as
	// an IP on the way in so that nothing else the far side printed can land
	// here.
	//
	// It is the one thing on this row that says which addresses are open. What
	// was asked for is on Local, and what the SSH server answered to each
	// request is on OpenReach, and neither is a binding: a server told to bind
	// every interface opens both families on the first request and refuses the
	// second, and it does that for a request that named a loopback address
	// too. The Host is the only place that knows, so it is asked.
	//
	// The empty value is that it was not asked or could not answer, and never
	// that nothing is listening. An account with no shell refuses the session
	// channel, a Host with neither ss nor netstat prints nothing this can
	// read, and both of those look exactly like a port that is closed from
	// here. Being unable to ask is the configuration this program recommends
	// rather than a fault, so a screen draws nothing at all from an empty
	// value.
	//
	// It carries no "not null" for the reason ServerBanner does not: the
	// column is added to installations whose rows were written before it
	// existed, and AutoMigrate fills those with NULL.
	ListenAddresses string `json:"listen_addresses"`
}

// CreateHostRequest registers a Host. The password is no longer required on its
// own: a Host is registered with a private key, with a password, or with both,
// and which of them is missing is decided in the handler rather than by a rule
// on one field, so that the refusal can say what to do about it.
type CreateHostRequest struct {
	Address       string `json:"address" validate:"required,hostname_rfc1123|ip"`
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
	// tunnel at all, and carrying everything is what tunnel-manager did before
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
	// SocksEnabled, SocksPort, SocksBindScope and SocksAllowedSources are the
	// SOCKS5 proxy of the Host, as Host holds them. A request that switches
	// the proxy on names its port; a request that leaves SocksEnabled out
	// registers a Host without one. An empty SocksBindScope is loopback on a
	// Host being added, and keeps what is stored on one being changed.
	SocksEnabled        bool   `json:"socks_enabled"`
	SocksPort           int    `json:"socks_port" validate:"omitempty,min=1,max=65535"`
	SocksBindScope      string `json:"socks_bind_scope" validate:"omitempty,oneof=loopback wildcard"`
	SocksAllowedSources string `json:"socks_allowed_sources"`
	// JumpHostIDs is the jump route of the Host: the ids of the registered
	// Hosts used as its jump hosts, in the order its SSH connection is carried
	// through them. Left out, the Host is reached directly.
	JumpHostIDs []uint `json:"jump_host_ids"`
}

// UpdateHostRequest changes a Host. A field the request leaves out is left as
// it is, the private key and its passphrase included: an empty box on the
// screen keeps the key that is stored rather than taking it away.
type UpdateHostRequest struct {
	Address       string `json:"address" validate:"omitempty,hostname_rfc1123|ip"`
	Port          *int   `json:"port" validate:"omitempty,min=1,max=65535"`
	User          string `json:"user" validate:"omitempty"`
	Password      string `json:"password" validate:"omitempty"`
	PrivateKey    string `json:"private_key" validate:"omitempty"`
	KeyPassphrase string `json:"key_passphrase" validate:"omitempty"`
	Description   string `json:"description"`
	Enabled       *bool  `json:"enabled"`
	// SocksEnabled, SocksPort and SocksAllowedSources are pointers, so that a
	// request that leaves one out keeps what is stored and one that sends
	// false, or an empty list, says so: an empty list of allowed sources lets
	// every address in, and is a value a request has to be able to ask for.
	// SocksBindScope left empty keeps the scope that is stored, the way Address and
	// User do; the wildcard is asked for by its word. A Host stored with no
	// scope keeps that only while its proxy runs before and after the change,
	// and is stored on loopback otherwise, which is what Host.SocksScope reads
	// it as.
	SocksEnabled        *bool   `json:"socks_enabled"`
	SocksPort           *int    `json:"socks_port" validate:"omitempty,min=1,max=65535"`
	SocksBindScope      string  `json:"socks_bind_scope" validate:"omitempty,oneof=loopback wildcard"`
	SocksAllowedSources *string `json:"socks_allowed_sources"`
	// JumpHostIDs is a pointer so that a request that leaves the route out
	// keeps the one that is stored, and one that sends an empty list takes it
	// away.
	JumpHostIDs *[]uint `json:"jump_host_ids"`
	// AccountPassword is the password of the account. It is read only where
	// the change moves a Host that trusts no key yet and carries a login: a
	// change of its address, its SSH port or its jump route.
	AccountPassword string `json:"account_password"`
}

type CreateServicePortRequest struct {
	ServiceAddress string `json:"service_address" validate:"required,hostname_rfc1123|ip"`
	ServicePort    int    `json:"service_port" validate:"required,min=1,max=65535"`
	LocalPort      int    `json:"local_port" validate:"required,min=1,max=65535"`
	Description    string `json:"description"`
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

// LocalForwardRequest creates or changes a local forward of a Host. The Host
// is named by the path rather than by the body. An empty BindScope is a scope
// left out, which a creation opens on the loopback and a change keeps as it is
// stored; it is not the wildcard an empty LocalForward.BindScope stands for.
type LocalForwardRequest struct {
	BindScope     string `json:"bind_scope" validate:"omitempty,oneof=loopback wildcard"`
	LocalPort     int    `json:"local_port" validate:"required,min=1,max=65535"`
	TargetAddress string `json:"target_address" validate:"required,hostname_rfc1123|ip"`
	TargetPort    int    `json:"target_port" validate:"required,min=1,max=65535"`
	Description   string `json:"description"`
	// AllowedSources is LocalForward.AllowedSources, a pointer for the reason
	// Enabled is one. A creation that leaves it out lets every address in, and
	// a change that leaves it out keeps what is stored.
	AllowedSources *string `json:"allowed_sources"`
	// Enabled is a pointer for the reason CreateHostRequest.Enabled is. A
	// creation that leaves it out makes a forward that runs, and a change that
	// leaves it out keeps what is stored.
	Enabled *bool `json:"enabled"`
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

// APIToken is a credential a script sends instead of the session cookie. It is
// made on the Settings screen by a session, and it opens only the routes of the
// scopes it was made with.
//
// The token itself is not stored. What is kept is the SHA-256 of it, which is
// enough because the token is 32 random bytes: there is nothing to guess from
// a hash of that, so a slow hash like the one the account password is kept
// under would cost every request a bcrypt compare and protect nothing more.
type APIToken struct {
	ID uint `gorm:"primaryKey;autoIncrement" json:"id"`
	// Name is what the operator calls the token, and what a change made with
	// it is logged under. It is unique so that a line in the log names one
	// token and not any of several.
	Name string `gorm:"uniqueIndex:idx_api_tokens_name;not null" json:"name"`
	// Hash is the SHA-256 of the token, in lower case hex. It never leaves the
	// process, the way the hash of the account password does not.
	Hash string `gorm:"uniqueIndex:idx_api_tokens_hash;not null" json:"-"`
	// Scopes is the list of scopes the token was made with, joined by commas.
	Scopes string `gorm:"not null" json:"-"`
	// ExpiresAt is when the token stops opening anything. NULL is a token that
	// never runs out.
	ExpiresAt *time.Time `json:"expires_at"`
	// LastUsedAt is when a request last came in with the token, to the minute.
	// NULL is a token that has not been used yet.
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// TableName names the table the way the plural of the model would, written out
// so that a rename of the type does not move the rows to a new table.
func (APIToken) TableName() string {
	return "api_tokens"
}
