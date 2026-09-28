package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/crypto"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// The four calls in this file carry a configuration from one installation to
// another. What they move is a file: an export hands one out and an import
// takes one back, so the operator decides where it is kept and for how long,
// and neither installation has to reach the other.
//
// Every one of them is a POST, an export included. The password that seals the
// file is in the body, and a password in a URL is written to the access log of
// this server, to the log of anything in front of it and to the history of the
// browser that asked. A GET would put it in all three.

// transferFormatVersion is the version of the layout of transferFile. It is
// written into every file and checked on the way back in, so that a file from a
// later version is refused with a word about what it is rather than read as far
// as it happens to parse. It is raised when a reader of this version would get
// a file of the newer one wrong, not whenever a field is added: a field that is
// only added arrives as its zero value in an older reader, which the import
// already treats as "not named in the file".
//
// Version 2 carries the id of every row, and an import puts what the file holds
// in place of what is stored rather than adding to it. A reader of version 1
// would read such a file as rows to add and skip, so the number went up.
const transferFormatVersion = 2

// transferFormatWithIDs is the first version whose files carry the id of every
// row. An import reads a file of an earlier version with ids counted from 1 in
// the order of the file.
const transferFormatWithIDs = 2

// transferKindTunnels and transferKindSettings name what a file holds. They are
// what keeps the tunnel configuration from being read in as the settings of the
// manager: the two are both JSON with a "content" object, and without a name in
// the file an import would have nothing to refuse but the shape of what it
// found.
const (
	transferKindTunnels  = "tunnels"
	transferKindSettings = "settings"
)

// transferFile is what is sealed with the password. It carries what it holds,
// the version of this layout, and when and by which version of tunnel-manager
// it was written, so that a file found a year later says what it is without the
// installation that wrote it.
//
// The content is kept as raw JSON and read once the kind is known. That is what
// lets the settings be read onto the stored ones, so a file that does not name
// a setting leaves that setting as it is, the way PUT /api/settings does.
type transferFile struct {
	Kind          string          `json:"kind"`
	FormatVersion int             `json:"format_version"`
	ExportedAt    time.Time       `json:"exported_at"`
	ExportedBy    string          `json:"exported_by"`
	Content       json.RawMessage `json:"content"`
}

// hostContent is one Host as it is carried in a file.
//
// The password, the private key and the passphrase are in the clear here. In
// the database they are sealed with the key of the installation that stored
// them, and that key stays on that machine, so a file carrying them as they are
// stored would open on no other installation. They are unsealed on the way out
// and sealed again with the key of the installation that takes them in.
//
// So the JSON inside the file holds the SSH password and the PEM private key of
// every Host as text. What keeps them is the password the whole file is sealed
// with, and nothing else. An unsealed export is the credentials of every Host,
// which is why there is no way to ask for one.
//
// The id and the two timestamps are carried, so that an import puts back the
// rows as they were: the jump routes and the assignments name a Host by its id.
// A file from before they were carried has none, and nil stands for that.
type hostContent struct {
	ID      uint   `json:"id,omitempty"`
	Address string `json:"address"`
	// IP is the name Address was carried under by the releases before it took
	// a host name as well as an address. It is read and never written, the way
	// BindAddress below is, so that a file one of them exported still opens,
	// and a file that names both is read by Address. What is made of it is in
	// tunnelsContent.readOldNames.
	IP            string `json:"ip,omitempty"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	PrivateKey    string `json:"private_key"`
	KeyPassphrase string `json:"key_passphrase"`
	// HostKey is the public key the SSH server of this Host is trusted on,
	// carried as models.Host holds it. It is not sealed with the key of an
	// installation the way the three above are, because it is not a secret,
	// so what goes into the file is what the row holds.
	//
	// It is in the file so that moving a configuration does not throw the
	// trust away. A Host reaches nothing until the key of its server has been
	// approved by a person, and an installation that took the Hosts in without
	// their keys would connect to none of them until somebody had approved
	// every one again, one at a time. The file already carries the SSH
	// password and the PEM private key of every Host, so leaving out a public
	// key protects nothing and costs that.
	//
	HostKey string `json:"host_key"`
	// PendingHostKey is the key some server presented on a connection that was
	// refused, waiting for a person to approve it. It is carried so that the
	// file puts back the configuration as it was, the question included. A
	// file from before it was carried has none.
	PendingHostKey string `json:"pending_host_key"`
	// BindAddress is read and never written. The release before this one kept
	// how far the forwarded ports of a Host reach on the Host itself, as one
	// address for all of them, and wrote it into the files it exported. The
	// answer lives on the assignment now, so the export writes it there
	// instead and leaves this field empty, which keeps it out of the file.
	//
	// It is still read, because a file from that release is the only place
	// that answer is written, and an import that passed over it would put an
	// installation that had every port of a Host on loopback back on the
	// wildcard. That is reach handed out by an import rather than by a person,
	// which is what database.fillBindScopes refuses to let an upgrade do, and
	// a file is the same question arriving by another road. What is made of it
	// is in importAssignments.
	BindAddress string `json:"bind_address,omitempty"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	// JumpHostIDs is the jump route of the Host: the ids of the Hosts of the
	// same file it is reached through, in order. It is read from a file that
	// carries ids and from no other.
	JumpHostIDs []uint `json:"jump_host_ids,omitempty"`
	// Assignments is which service ports this Host carries, named by the id of
	// the service port in the same file. A file that carries ids is read by
	// this field and not by the three below, which are what a file from before
	// the ids carries, and the export no longer writes.
	Assignments []assignmentContent `json:"assignments,omitempty"`
	// AssignedLocalPorts is which service ports this Host carries, named by
	// their local port rather than by the id of the row. The ids belong to the
	// installation the file came from, so a file carrying them would point at
	// whatever happens to hold those ids here. The local port is unique across
	// the service ports of an installation (models.ServicePort, the
	// idx_service_local_port index), which is what makes it a name that means
	// the same on both sides.
	//
	// nil and an empty list mean different things, and the difference is what a
	// file written before the assignments were stored turns on:
	//
	//	no field, or null  this Host carries every service port in the file
	//	[]                 this Host carries none of them
	//
	// Every Host carrying every service port is what an installation ran on
	// before the assignments were a row of their own, so it is what a file from
	// then meant without saying it. Read as "carries none", such a file would
	// import cleanly and leave the installation with no tunnel at all.
	//
	// encoding/json is what tells the two apart. An absent field and a null
	// both leave the field at nil, while "[]" sets it to a slice of length zero
	// that is not nil. The export therefore never writes nil: a Host that
	// carries nothing goes into the file as "[]", because a nil slice marshals
	// to null, which reads back as the file not naming the assignments at all.
	AssignedLocalPorts []int `json:"assigned_local_ports,omitempty"`
	// AssignedBindScopes is how far each of those assignments reaches, under
	// the local port it is named by above, written as text because that is
	// what a JSON object has for a key. The local port is what means the same
	// on both installations, so the scopes are keyed by it for the reason the
	// assignments themselves are named by it.
	//
	// An assignment with no entry here is on the wildcard, which is what an
	// empty column means (models.HostServicePort), so the export writes an
	// entry only where there is an answer other than that one. A file from
	// before this field carries none at all, and the Host-wide bind address
	// above is what such a file says instead.
	//
	// It is a second field rather than assigned_local_ports becoming a list of
	// objects, so that a reader of the release before this one goes on reading
	// which service ports a Host carries out of the file it knows. Changing
	// the shape of that field would have it read a Host that carries nothing
	// as a Host that carries everything, which is what transferFormatVersion
	// would then have to be raised for.
	AssignedBindScopes map[string]string `json:"assigned_bind_scopes,omitempty"`
	// AssignedEnabled is whether the tunnel of each of those assignments runs,
	// keyed the way AssignedBindScopes is. An assignment with no entry runs: a
	// file from before assignments could be switched off carries no field, and
	// every assignment in it was running. The export therefore writes an entry
	// only for an assignment that is switched off, the way it writes a scope
	// only where there is one other than the wildcard.
	//
	// It is a field of its own for the reason AssignedBindScopes is. A reader
	// of the release before this one passes over it and imports every
	// assignment switched on, which is what that release can run.
	AssignedEnabled map[string]bool `json:"assigned_enabled,omitempty"`
	// LocalForwards are the local forwards this Host carries, in the order of
	// their number. A file from before they were stored carries no field, and
	// the Host is imported forwarding nothing.
	LocalForwards []localForwardContent `json:"local_forwards"`
	// SocksEnabled, SocksPort, SocksBindScope and SocksAllowedSources are the
	// SOCKS5 proxy of the Host. Each is a pointer, so that a field the file
	// does not carry is told from one it carries as false, zero or empty. A
	// file from before the proxies were stored carries none of them, and the
	// Host is imported with none. The export writes all four every time.
	SocksEnabled        *bool   `json:"socks_enabled"`
	SocksPort           *int    `json:"socks_port"`
	SocksBindScope      *string `json:"socks_bind_scope"`
	SocksAllowedSources *string `json:"socks_allowed_sources"`

	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// assignmentContent is one service port a Host carries, as a file with ids
// carries it: the id of the service port, how far it reaches, whether it runs,
// and when it was made.
type assignmentContent struct {
	ServicePortID uint       `json:"service_port_id"`
	BindScope     string     `json:"bind_scope"`
	Enabled       bool       `json:"enabled"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
}

// socksOf lays the SOCKS5 fields a file carries for a Host over the ones the
// Host is stored with, and leaves a field the file does not carry as stored.
// An empty scope is stored as the word it stands for.
func (host hostContent) socksOf(stored models.Host) models.Host {
	if host.SocksEnabled != nil {
		stored.SocksEnabled = *host.SocksEnabled
	}
	if host.SocksPort != nil {
		stored.SocksPort = *host.SocksPort
	}
	if host.SocksBindScope != nil {
		stored.SocksBindScope = *host.SocksBindScope
		if stored.SocksBindScope == "" {
			stored.SocksBindScope = models.BindScopeWildcard
		}
	}
	if host.SocksAllowedSources != nil {
		stored.SocksAllowedSources = *host.SocksAllowedSources
	}

	return stored
}

// opensSocks reports whether the file switches the SOCKS5 proxy of this Host
// on with a port, which is when the port is held to what this installation
// opens.
func (host hostContent) opensSocks() bool {
	return host.SocksEnabled != nil && *host.SocksEnabled && host.SocksPort != nil && *host.SocksPort > 0
}

// localForwardContent is one local forward as it is carried in a file, on the
// Host it belongs to. The number and the timestamps are carried for the reason
// the id of a Host is. The Host is the one the forward is written under.
type localForwardContent struct {
	Number        uint   `json:"number,omitempty"`
	BindScope     string `json:"bind_scope"`
	LocalPort     int    `json:"local_port"`
	TargetAddress string `json:"target_address"`
	// TargetIP is the name TargetAddress was carried under, read and never
	// written for the reason hostContent.IP is.
	TargetIP    string `json:"target_ip,omitempty"`
	TargetPort  int    `json:"target_port"`
	Description string `json:"description"`
	// Enabled is a pointer so that a file from before a forward could be
	// switched off, which carries no field, is told from one that carries
	// false. Every forward of such a file was running, so it is stored as on.
	// The export writes it every time.
	Enabled *bool `json:"enabled"`
	// AllowedSources is models.LocalForward.AllowedSources. A file from before
	// the list existed carries no field, which reads as empty: every address
	// let in, as every forward of such a file did.
	AllowedSources string `json:"allowed_sources"`

	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// enabled is whether the forward is stored as switched on.
func (lf localForwardContent) enabled() bool {
	return lf.Enabled == nil || *lf.Enabled
}

// servicePortContent is one service port as it is carried in a file. It holds
// no secret, and the id and the timestamps are carried for the reason they are
// carried for a Host.
type servicePortContent struct {
	ID             uint   `json:"id,omitempty"`
	ServiceAddress string `json:"service_address"`
	// ServiceIP is the name ServiceAddress was carried under, read and never
	// written for the reason hostContent.IP is.
	ServiceIP   string `json:"service_ip,omitempty"`
	ServicePort int    `json:"service_port"`
	LocalPort   int    `json:"local_port"`
	Description string `json:"description"`

	CreatedAt *time.Time `json:"created_at,omitempty"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// tunnelsContent is the content of a file of kind tunnels.
//
// The tunnels table is not in it. A row there is the state of one connection of
// this installation, which the reconcile loop writes from the Hosts and the
// service ports; carried across, it would describe connections the other
// installation never made.
//
// The assignments are in it, on each Host, and they are held to the other side
// of that same rule: which service ports a Host is to carry is what the
// operator asked for rather than what this installation made of it, so an
// installation that does not carry it across comes up running something else.
type tunnelsContent struct {
	Hosts        []hostContent        `json:"hosts"`
	ServicePorts []servicePortContent `json:"service_ports"`
}

// readOldNames moves every address a file carries under the name an earlier
// release wrote it with onto the name it is read by now, so that nothing past
// the decoding has to know there were two. Where a file names both, the new
// name is the one kept: a file carrying it was written by a release that
// reads it, and the old one beside it can only be left over. The old names are
// emptied on the way, which also keeps them out of anything written from the
// content afterwards.
func (content *tunnelsContent) readOldNames() {
	for i := range content.Hosts {
		host := &content.Hosts[i]
		if host.Address == "" {
			host.Address = host.IP
		}
		host.IP = ""

		for j := range host.LocalForwards {
			forward := &host.LocalForwards[j]
			if forward.TargetAddress == "" {
				forward.TargetAddress = forward.TargetIP
			}
			forward.TargetIP = ""
		}
	}

	for i := range content.ServicePorts {
		sp := &content.ServicePorts[i]
		if sp.ServiceAddress == "" {
			sp.ServiceAddress = sp.ServiceIP
		}
		sp.ServiceIP = ""
	}
}

// settingsContent is the content of a file of kind settings.
//
// It is spelled out rather than being settings.Settings itself, so that the row
// id and the time the row was last written stay out of the file: both describe
// the row this export was read from. A setting added to settings.Settings and
// not added here would quietly not be carried, which is what
// TestTheSettingsContentCarriesEverySetting is for.
type settingsContent struct {
	APIPort int `json:"api_port"`
	// api.port and api.https_enabled are carried like every other setting. An
	// import stores them and nothing more, so the port this process is
	// listening on does not move under the request that is being answered; the
	// stored value is what the next startup listens on, and until then it is
	// reported by GET /api/settings as waiting for a restart.
	APIHTTPSEnabled       bool   `json:"api_https_enabled"`
	MonitoringIntervalSec int    `json:"monitoring_interval_sec"`
	ReconcileIntervalSec  int    `json:"reconcile_interval_sec"`
	SecurityKeyFile       string `json:"security_key_file"`
	LoggingLevel          string `json:"logging_level"`
	LoggingFormat         string `json:"logging_format"`
	LoggingFilePath       string `json:"logging_file_path"`
	LoggingFileMaxSize    int    `json:"logging_file_max_size"`
	LoggingFileMaxBackups int    `json:"logging_file_max_backups"`
	LoggingFileMaxAge     int    `json:"logging_file_max_age"`
	LoggingFileCompress   bool   `json:"logging_file_compress"`
	// UIDefaultLanguage travels with the rest. It names a language rather
	// than a machine, so it means the same on the installation that takes the
	// file in, and an empty value carries across as the empty value it is.
	UIDefaultLanguage string `json:"ui_default_language"`
	// The update settings travel too. Whether to look for a release is a
	// choice about this installation rather than about the machine it is on,
	// so it means the same wherever the file is taken in.
	//
	// The one to be careful with is the automatic install. A file exported
	// from an installation that has it on turns it on wherever it is imported,
	// and what it does there is take that service down when a release appears.
	// It is carried all the same: a setting left out of the file is one that
	// silently keeps whatever the other installation had, which is the worse
	// of the two surprises, and an import is already a thing that replaces
	// what is stored.
	UpdateCheckEnabled       bool `json:"update_check_enabled"`
	UpdateCheckIntervalHours int  `json:"update_check_interval_hours"`
	UpdateAutoInstall        bool `json:"update_auto_install"`

	// ReconnectMaxIntervalSec is carried like the monitoring interval it goes
	// with. A file exported before the setting existed leaves it as it is
	// stored here, since the content is read onto the stored settings.
	ReconnectMaxIntervalSec int `json:"reconnect_max_interval_sec"`

	// The alert settings travel with the rest, and a file exported before
	// they existed leaves them as they are stored, for the reason above.
	AlertAfterSec   int    `json:"alert_after_sec"`
	AlertWebhookURL string `json:"alert_webhook_url"`
	SMTPHost        string `json:"smtp_host"`
	SMTPPort        int    `json:"smtp_port"`
	SMTPSecurity    string `json:"smtp_security"`
	SMTPAuth        string `json:"smtp_auth"`
	SMTPUsername    string `json:"smtp_username"`
	// SMTPPassword is the password of the mail server in the clear, the way
	// the passwords of the Hosts are carried: sealed with the key of this
	// installation it would be bytes only this system can open. The file as a
	// whole is sealed with the export password.
	//
	// It is a pointer so that a file that carries no password is told from
	// one that carries an empty one. The first leaves the stored password as
	// it is, which is what a file from before the setting does; the second
	// removes it, which is what the installation that wrote the file had.
	// Nothing but the export fills it in, since settingsOf has no key to open
	// the stored one with.
	SMTPPassword   *string `json:"smtp_password,omitempty"`
	SMTPFrom       string  `json:"smtp_from"`
	SMTPTo         string  `json:"smtp_to"`
	SMTPSkipVerify bool    `json:"smtp_skip_verify"`
}

// settingsOf returns the settings of a set as they are carried in a file.
func settingsOf(s *settings.Settings) settingsContent {
	return settingsContent{
		APIPort:               s.APIPort,
		APIHTTPSEnabled:       s.APIHTTPSEnabled,
		MonitoringIntervalSec: s.MonitoringIntervalSec,
		ReconcileIntervalSec:  s.ReconcileIntervalSec,
		SecurityKeyFile:       s.SecurityKeyFile,
		LoggingLevel:          s.LoggingLevel,
		LoggingFormat:         s.LoggingFormat,
		LoggingFilePath:       s.LoggingFilePath,
		LoggingFileMaxSize:    s.LoggingFileMaxSize,
		LoggingFileMaxBackups: s.LoggingFileMaxBackups,
		LoggingFileMaxAge:     s.LoggingFileMaxAge,
		LoggingFileCompress:   s.LoggingFileCompress,
		UIDefaultLanguage:     s.UIDefaultLanguage,

		UpdateCheckEnabled:       s.UpdateCheckEnabled,
		UpdateCheckIntervalHours: s.UpdateCheckIntervalHours,
		UpdateAutoInstall:        s.UpdateAutoInstall,

		ReconnectMaxIntervalSec: s.ReconnectMaxIntervalSec,

		AlertAfterSec:   s.AlertAfterSec,
		AlertWebhookURL: s.AlertWebhookURL,
		SMTPHost:        s.SMTPHost,
		SMTPPort:        s.SMTPPort,
		SMTPSecurity:    s.SMTPSecurity,
		SMTPAuth:        s.SMTPAuth,
		SMTPUsername:    s.SMTPUsername,
		SMTPFrom:        s.SMTPFrom,
		SMTPTo:          s.SMTPTo,
		SMTPSkipVerify:  s.SMTPSkipVerify,
	}
}

// applyTo puts what the file carries onto a set of settings, leaving the id and
// the timestamp of that set alone.
func (content *settingsContent) applyTo(s *settings.Settings) {
	s.APIPort = content.APIPort
	s.APIHTTPSEnabled = content.APIHTTPSEnabled
	s.MonitoringIntervalSec = content.MonitoringIntervalSec
	s.ReconnectMaxIntervalSec = content.ReconnectMaxIntervalSec
	s.ReconcileIntervalSec = content.ReconcileIntervalSec
	s.SecurityKeyFile = content.SecurityKeyFile
	s.LoggingLevel = content.LoggingLevel
	s.LoggingFormat = content.LoggingFormat
	s.LoggingFilePath = content.LoggingFilePath
	s.LoggingFileMaxSize = content.LoggingFileMaxSize
	s.LoggingFileMaxBackups = content.LoggingFileMaxBackups
	s.LoggingFileMaxAge = content.LoggingFileMaxAge
	s.LoggingFileCompress = content.LoggingFileCompress
	s.UIDefaultLanguage = content.UIDefaultLanguage
	s.UpdateCheckEnabled = content.UpdateCheckEnabled
	s.UpdateCheckIntervalHours = content.UpdateCheckIntervalHours
	s.UpdateAutoInstall = content.UpdateAutoInstall
	s.AlertAfterSec = content.AlertAfterSec
	s.AlertWebhookURL = content.AlertWebhookURL
	s.SMTPHost = content.SMTPHost
	s.SMTPPort = content.SMTPPort
	s.SMTPSecurity = content.SMTPSecurity
	s.SMTPAuth = content.SMTPAuth
	s.SMTPUsername = content.SMTPUsername
	s.SMTPFrom = content.SMTPFrom
	s.SMTPTo = content.SMTPTo
	s.SMTPSkipVerify = content.SMTPSkipVerify
	// The password is left to the import, which holds the key it is sealed
	// with. See settingsContent.SMTPPassword.
}

// exportRequest is what an export is asked for. The password seals the file and
// is the only thing that opens it again: it is not stored anywhere, so a
// forgotten one leaves the file unreadable.
//
// AccountPassword is the password of the account, asked for again. The file
// carries in the clear what the database keeps sealed, and a session left open
// on a screen, or a token, is not to be all it takes to carry that away under a
// password the caller picks.
type exportRequest struct {
	Password        string `json:"password"`
	AccountPassword string `json:"account_password"`
}

// importRequest is what an import is given: the file as the export handed it
// out, and the password it was sealed with.
type importRequest struct {
	Password string `json:"password"`
	File     string `json:"file"`
}

// importTunnelsRequest is what the import of the tunnels is given: what every
// import is given, and the password of the account. The import puts the host
// keys of the file in place of the ones trusted here, which is what the
// approval on the Status screen asks the password for, so the import asks for
// it too. The import of the settings is left without it and takes
// importRequest.
//
// DryRun asks for the file to be opened and checked and nothing written, so
// that a screen can say what the import would replace before it is made.
//
// overwrite, what the import that added and skipped rows was told whether to
// replace the rows it met by, has no field here. ImportTunnels refuses a body
// that carries the key before the body is decoded, whatever its value is.
type importTunnelsRequest struct {
	importRequest
	AccountPassword string `json:"account_password"`
	DryRun          bool   `json:"dry_run"`
}

// exportedTunnels is the answer to an export of the tunnel configuration. The
// counts are there so that the operator can see what went into the file without
// opening it, which takes the password.
type exportedTunnels struct {
	Kind          string    `json:"kind"`
	File          string    `json:"file"`
	ExportedAt    time.Time `json:"exported_at"`
	Hosts         int       `json:"hosts"`
	ServicePorts  int       `json:"service_ports"`
	LocalForwards int       `json:"local_forwards"`
}

// exportedSettings is the answer to an export of the settings of the manager.
type exportedSettings struct {
	Kind       string    `json:"kind"`
	File       string    `json:"file"`
	ExportedAt time.Time `json:"exported_at"`
}

// transferSkipped is what an import of the settings did with a path of the
// file it did not store.
const transferSkipped = "skipped"

// transferItem is one thing of the file and what the import did with it. The
// name is how the operator finds it on the screens.
//
// A name or a reason that is an English sentence is named beside it by a
// textCode and the values written into it, the way a refusal is, so that a
// screen says it in the language it is drawn in. The code is left out for a
// name that is only an address or the name of a setting.
type transferItem struct {
	Kind         string   `json:"kind"`
	Name         string   `json:"name"`
	NameCode     textCode `json:"name_code,omitempty"`
	NameValues   textArgs `json:"name_values,omitempty"`
	Action       string   `json:"action"`
	Reason       string   `json:"reason,omitempty"`
	ReasonCode   textCode `json:"reason_code,omitempty"`
	ReasonValues textArgs `json:"reason_values,omitempty"`
}

// The names and the reasons an item or a refusal of an import is written with.
const (
	textImportNameServicePort textCode = "import.name.service_port"

	textImportReasonPathOutside      textCode = "import.reason.path_outside"
	textImportReasonEmptyPathOutside textCode = "import.reason.empty_path_outside"
)

// transferTexts is the English of each of those codes, written with {name} for
// a value the way errorMessages is. This is the only place they are written:
// the English an item carries is this sentence filled in, so it cannot say
// something other than what the code stands for.
//
// An empty path has a sentence of its own rather than a phrase written into the
// other one, because "an empty path" is English a translator would not be
// handed.
var transferTexts = map[textCode]string{
	textImportNameServicePort: "{service_address} on {local_port}",

	textImportReasonPathOutside: "the file carries {carried}, which does not name a file under the " +
		"directory the database file is in, so {stored} was stored instead",
	textImportReasonEmptyPathOutside: "the file carries an empty path, which does not name a file under the " +
		"directory the database file is in, so {stored} was stored instead",
}

// transferText is one of those sentences with its values.
type transferText struct {
	code   textCode
	values textArgs
}

// english is the sentence filled in.
func (t transferText) english() string {
	english, _ := renderErrorMessage(transferTexts[t.code], errorArgs(t.values))

	return english
}

// namedBy writes the name of the item from a sentence.
func (item transferItem) namedBy(name transferText) transferItem {
	item.Name = name.english()
	item.NameCode = name.code
	item.NameValues = name.values

	return item
}

// because writes the reason of the item from a sentence.
func (item transferItem) because(reason transferText) transferItem {
	item.Reason = reason.english()
	item.ReasonCode = reason.code
	item.ReasonValues = reason.values

	return item
}

// transferCounts is how much of the tunnel configuration one side holds.
// JumpHosts is the number of Hosts that have a jump route.
type transferCounts struct {
	Hosts         int `json:"hosts"`
	ServicePorts  int `json:"service_ports"`
	Assignments   int `json:"assignments"`
	LocalForwards int `json:"local_forwards"`
	JumpHosts     int `json:"jump_hosts"`
}

// importedTunnels is the answer to an import of the tunnel configuration:
// what was stored here when it was asked for, which an import deletes, and
// what the file holds, which an import writes. A dry run answers the same and
// writes nothing.
type importedTunnels struct {
	DryRun  bool           `json:"dry_run"`
	Current transferCounts `json:"current"`
	File    transferCounts `json:"file"`
}

// The two settings that name a file of this installation, as the file carries
// them and as settings.Diff names them. They are the only two an import cannot
// store as the file has them, because what they may hold depends on the
// installation reading the file rather than on the one that wrote it: both are
// read against the directory the database file is in, and the settings package
// refuses one that names a place outside it.
//
// The pair is read and written through functions rather than through a pointer
// into a settings.Settings, because the check below needs the same field of a
// second set: a path is offered to the rule by putting it into a set that
// passes every other one.
var importedDataPaths = []struct {
	name  string
	read  func(*settings.Settings) string
	write func(*settings.Settings, string)
}{
	{
		name:  "security.key_file",
		read:  func(s *settings.Settings) string { return s.SecurityKeyFile },
		write: func(s *settings.Settings, path string) { s.SecurityKeyFile = path },
	},
	{
		name:  "logging.file.path",
		read:  func(s *settings.Settings) string { return s.LoggingFilePath },
		write: func(s *settings.Settings, path string) { s.LoggingFilePath = path },
	},
}

// acceptsDataPath asks the settings package whether it would store a path in
// the field the setter writes.
//
// It asks by holding the rule against a set of the defaults with that one
// field changed, because the rule itself is not reachable from here: the
// settings package keeps it unexported and runs it inside Validate. A copy of
// it written out in this file would be a second place deciding what a stored
// path may be, and the two would part company the first time the rule moves,
// leaving the import storing what the startup then repairs behind the
// operator's back. The defaults pass every other rule, so a set that is
// refused here is refused for the path and for nothing else.
func acceptsDataPath(write func(*settings.Settings, string), path string) bool {
	probe := settings.Defaults()
	write(&probe, path)

	return probe.Validate() == nil
}

// dropPathsOutsideTheInstallation puts a path the settings package refuses back
// to its default and reports the ones it dropped.
//
// An export written before those two settings were held inside the
// installation directory carries an absolute path, which is what every
// installation that was set up by hand has: it was accepted when the file was
// written. Stored as it is, it is refused, and the whole import fails - so a
// file that carries the Hosts, the intervals and the language across would
// move none of it because of where a log file used to go. Moving a
// configuration is the one thing this call is for, and it is the way out of an
// installation that has just been refused for that same path.
//
// So the path is dropped to the default and the rest of the file is stored,
// which is what the startup does with a stored path it finds (settings.
// RepairPaths). The two agree on purpose: an operator who upgrades this
// installation and one who imports the settings of it somewhere else end up
// running on the same thing. What was dropped is in the answer rather than
// only in the log, because the operator is standing in front of the screen
// that asked for the import and the path that was thrown away may be the one
// the encryption key of the other installation is under.
//
// What it hands back is what RepairPaths hands back, a settings.Change per
// path, so that the answer and the log line are written from one reading of
// what happened rather than from two.
func dropPathsOutsideTheInstallation(s *settings.Settings) []settings.Change {
	defaults := settings.Defaults()

	dropped := make([]settings.Change, 0, len(importedDataPaths))

	for _, path := range importedDataPaths {
		carried := path.read(s)
		if acceptsDataPath(path.write, carried) {
			continue
		}

		fallback := path.read(&defaults)
		path.write(s, fallback)

		dropped = append(dropped, settings.Change{
			Name: path.name,
			From: carried,
			To:   fallback,
		})
	}

	return dropped
}

// droppedPathItems says in the answer what became of each path that was
// dropped, as a row of the same list an import of the tunnel configuration
// answers with: what it is, which setting, that the file did not get its way,
// and why.
//
// A file that carries an empty path is named as carrying one. It is refused by
// the same rule, since neither the log nor the key file can be turned off by
// leaving it out, and a sentence with nothing where the path should be reads
// as though the reason had been written wrong.
func droppedPathItems(dropped []settings.Change) []transferItem {
	items := make([]transferItem, 0, len(dropped))

	for _, change := range dropped {
		reason := transferText{textImportReasonPathOutside,
			textArgs{"carried": change.From, "stored": change.To}}
		if change.From == "" {
			reason = transferText{textImportReasonEmptyPathOutside, textArgs{"stored": change.To}}
		}

		items = append(items, transferItem{
			Kind:   "setting",
			Name:   change.Name,
			Action: transferSkipped,
		}.because(reason))
	}

	return items
}

// importedSettings is the answer to an import of the settings of the manager.
// It carries what is stored now and what the import changed, the way a save on
// the Settings screen does.
//
// RestartRequired is true whenever anything changed, because nothing here is
// put into place on the running process. What is waiting is listed by
// GET /api/settings, which works it out by holding the stored settings against
// the ones this process started on.
//
// Items is what the import did not take from the file, carried the way an
// import of the tunnel configuration carries it: one row per thing, with the
// reason on the ones that were skipped. Only the two path settings can land
// there, and Changes is not the place for them, because a change says what the
// stored value went from and to while these say what the file asked for and
// did not get - a file whose path is dropped onto the value this installation
// already holds changes nothing and still has to be reported.
type importedSettings struct {
	Settings        *settings.Settings `json:"settings"`
	Changes         []settingsChange   `json:"changes"`
	Items           []transferItem     `json:"items"`
	RestartRequired bool               `json:"restart_required"`
}

// TransferHandler serves the four calls.
//
// It holds the Handler that serves the Host screens rather than a database
// handle and a cipher of its own. An import writes the rows a create writes and
// has to seal their secrets exactly as a create seals them, so it goes through
// the same sealPassword and sealPrivateKey: a second copy of that code here
// would be a second place that decides what a stored Host looks like, and a
// Host stored in any other shape is one the tunnels cannot open.
//
// version is the version of this binary, handed in from the startup that knows
// it, and is written into the file so that a file says what wrote it.
type TransferHandler struct {
	hosts   *Handler
	version string
	// databaseFile is the database file the process was started with, which
	// imported settings are held against for the reason SettingsHandler holds
	// a save against it.
	databaseFile string
}

func NewTransferHandler(hosts *Handler, version string, databaseFile string) *TransferHandler {
	return &TransferHandler{
		hosts:        hosts,
		version:      version,
		databaseFile: databaseFile,
	}
}

// accountPasswordRefused checks the password of the account an export or an
// import of the tunnels is asked for with, and returns what to answer with when
// it does not open the account.
//
// It is called before anything else of the call runs. The file is not read,
// sealed or opened for a caller who has not shown the password, and scrypt,
// which the sealing and the opening run, is not to be something such a caller
// can make the server spend its CPU on.
//
// An empty box is refused before the check, so that a press with nothing typed
// is not answered as a password that is wrong nor counted as a guess.
//
// importing tells the import of the tunnels from the two exports, which are
// refused under codes of their own so that the screen says what was not done.
// kind is what the call moves, for the log line.
func (h *TransferHandler) accountPasswordRefused(c echo.Context, password string, importing bool,
	kind string) *refusal {
	required, wrong := errExportAccountPasswordRequired, errExportAccountPasswordWrong
	if importing {
		required, wrong = errImportAccountPasswordRequired, errImportAccountPasswordWrong
	}

	if password == "" {
		return refuse(http.StatusBadRequest, required)
	}

	refused := accountPasswordRefused(c, h.hosts.db, h.hosts.logger, password, wrong)
	if refused == nil || refused.code != wrong {
		return refused
	}

	if importing {
		h.hosts.logger.Warn("an import was asked for with a password that does not open the account. "+
			"Nothing was read or written",
			logid.TransferImportAccountPasswordWrong.Field(), zap.String("kind", kind))
	} else {
		h.hosts.logger.Warn("an export was asked for with a password that does not open the account. "+
			"No file was made",
			logid.TransferExportAccountPasswordWrong.Field(), zap.String("kind", kind))
	}

	return refused
}

// checkExportPassword holds the password that seals a file to the length the
// account is held to. The file carries the SSH credentials of every Host and is
// kept wherever the operator puts it, so it stands to be guessed at for as long
// as it exists, which is longer than a login does.
func checkExportPassword(password string) *refusal {
	switch {
	case password == "":
		return refuse(http.StatusBadRequest, errExportPasswordRequired)
	case len(password) < minPasswordBytes:
		return refuse(http.StatusBadRequest, errExportPasswordTooShort, errorArgs{"min": strconv.Itoa(minPasswordBytes)})
	case len(password) > maxPasswordBytes:
		return refuse(http.StatusBadRequest, errExportPasswordTooLong, errorArgs{"max": strconv.Itoa(maxPasswordBytes)})
	}

	return nil
}

// seal builds the file and seals it with the password.
func (h *TransferHandler) seal(kind string, content interface{}, password string,
	exportedAt time.Time) (string, error) {
	body, err := json.Marshal(content)
	if err != nil {
		return "", err
	}

	file := transferFile{
		Kind:          kind,
		FormatVersion: transferFormatVersion,
		ExportedAt:    exportedAt,
		ExportedBy:    h.version,
		Content:       body,
	}

	plaintext, err := json.Marshal(file)
	if err != nil {
		return "", err
	}

	return crypto.EncryptWithPassword(string(plaintext), password)
}

// open unseals a file and reads it, and says in the answer which of the four
// ways it failed. They are kept apart because they leave the operator with
// different work to do: type the password again, pick another file, fetch the
// file again because this copy is cut, or go to the other import.
func (h *TransferHandler) open(file string, password string, want string) (*transferFile, *refusal) {
	if strings.TrimSpace(file) == "" {
		return nil, refuse(http.StatusBadRequest, errImportFileMissing)
	}

	plaintext, err := crypto.DecryptWithPassword(strings.TrimSpace(file), password)
	if err != nil {
		switch {
		case errors.Is(err, crypto.ErrPasswordRequired):
			return nil, refuse(http.StatusBadRequest, errImportPasswordRequired)
		case errors.Is(err, crypto.ErrNotPasswordEncrypted):
			return nil, refuse(http.StatusBadRequest, errImportFileNotAnExport)
		case errors.Is(err, crypto.ErrPasswordEncryptedDamaged):
			return nil, refuse(http.StatusBadRequest, errImportFileDamaged)
		case errors.Is(err, crypto.ErrWrongPassword):
			return nil, refuse(http.StatusBadRequest, errImportPasswordWrong)
		}

		// Everything above is what DecryptWithPassword reports. Anything else
		// is a failure of this process rather than of the file, so it is logged
		// and answered as one.
		h.hosts.logger.Error("failed to open an exported file", logid.TransferExportedFileOpenFailed.Field(), zap.Error(err))

		return nil, refuse(http.StatusInternalServerError, errImportFileOpenFailed)
	}

	var read transferFile

	err = json.Unmarshal([]byte(plaintext), &read)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, errImportFileNotOurs)
	}

	if read.Kind != want {
		found, foundCode := whatIsIn(read.Kind)
		wanted, wantedCode := whatIsIn(want)

		// The kind the file names is handed over on its own for the one code
		// whose phrase says it. It is the value of a field of the file, not a
		// sentence, so there is nothing to translate in it.
		values := textArgs(nil)
		if foundCode == textImportKindUnknown {
			values = textArgs{"kind": read.Kind}
		}

		return nil, refuse(http.StatusBadRequest, errImportFileWrongKind, errorArgs{"found": found, "wanted": wanted}).
			named(map[string]textCode{"found": foundCode, "wanted": wantedCode}, values)
	}

	if read.FormatVersion > transferFormatVersion {
		version := strconv.Itoa(read.FormatVersion)
		supported := strconv.Itoa(transferFormatVersion)

		// A file that says which version wrote it is refused under a code of
		// its own rather than under the one below with an empty value. The
		// version is in the middle of the sentence, and a sentence with a hole
		// where it should be is not one a screen can write in its own language.
		if read.ExportedBy != "" {
			return nil, refuse(http.StatusBadRequest, errImportFileNewerFormatBy, errorArgs{
				"version":     version,
				"supported":   supported,
				"exported_by": read.ExportedBy,
			})
		}

		return nil, refuse(http.StatusBadRequest, errImportFileNewerFormat, errorArgs{
			"version":   version,
			"supported": supported,
		})
	}

	if len(read.Content) == 0 {
		return nil, refuse(http.StatusBadRequest, errImportFileEmpty)
	}

	return &read, nil
}

// The four things a file can hold, as a refusal says them. The last one is the
// phrase for a kind this version has no name for, and it is written with the
// kind the file names under "kind".
const (
	textImportKindTunnels  textCode = "import.kind.tunnels"
	textImportKindSettings textCode = "import.kind.settings"
	textImportKindNone     textCode = "import.kind.none"
	textImportKindUnknown  textCode = "import.kind.unknown"
)

// whatIsIn names a kind the way it is said in a refusal, in English and by
// code.
func whatIsIn(kind string) (string, textCode) {
	switch kind {
	case transferKindTunnels:
		return "the tunnel configuration", textImportKindTunnels
	case transferKindSettings:
		return "the settings of the manager", textImportKindSettings
	case "":
		return "nothing this version knows", textImportKindNone
	}

	return "a kind this version does not know (" + kind + ")", textImportKindUnknown
}

// exportedBy names the version that wrote a file, when the file says.
func exportedBy(version string) string {
	if version == "" {
		return ""
	}

	return " (" + version + ")"
}

// unseal returns the plaintext of a value the database holds sealed with the
// key of this installation. An empty value stays empty, and a value stored
// before the secrets were sealed at all is carried as it is: ErrNotEncrypted
// means the column holds the plaintext already.
func (h *TransferHandler) unseal(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}

	plaintext, err := h.hosts.cipher.Decrypt(stored)
	if errors.Is(err, crypto.ErrNotEncrypted) {
		return stored, nil
	}
	if err != nil {
		return "", err
	}

	return plaintext, nil
}

// unsealHost returns one Host as it is carried in a file.
//
// The three secrets are opened with the key of this installation here, so that
// what goes into the file is the plaintext. Left sealed, they would be bytes
// only this machine can read, and the Hosts would arrive at the other
// installation with credentials nothing there can use.
func (h *TransferHandler) unsealHost(host models.Host) (hostContent, error) {
	password, err := h.unseal(host.Password)
	if err != nil {
		return hostContent{}, err
	}

	privateKey, err := h.unseal(host.PrivateKey)
	if err != nil {
		return hostContent{}, err
	}

	keyPassphrase, err := h.unseal(host.KeyPassphrase)
	if err != nil {
		return hostContent{}, err
	}

	createdAt := host.CreatedAt
	updatedAt := host.UpdatedAt

	return hostContent{
		ID:             host.ID,
		Address:        host.Address,
		Port:           host.Port,
		User:           host.User,
		Password:       password,
		PrivateKey:     privateKey,
		KeyPassphrase:  keyPassphrase,
		HostKey:        host.HostKey,
		PendingHostKey: host.PendingHostKey,
		Description:    host.Description,
		Enabled:        host.Enabled,

		SocksEnabled:        &host.SocksEnabled,
		SocksPort:           &host.SocksPort,
		SocksBindScope:      &host.SocksBindScope,
		SocksAllowedSources: &host.SocksAllowedSources,

		CreatedAt: &createdAt,
		UpdatedAt: &updatedAt,
	}, nil
}

// assignedByHost reads the assignments and returns them for each Host id, in
// the order of the id of the service port so that exporting the same
// configuration twice gives the same file. The service ports are handed in
// rather than read again, since the export has them already and the two reads
// have to agree on what is stored.
//
// An assignment whose service port is not among them is left out. It points at
// a row that is not there, and the import refuses a file whose Host carries a
// service port the file does not hold.
func assignedByHost(db *gorm.DB, sps []models.ServicePort) (map[uint][]assignmentContent, error) {
	var assignments []models.HostServicePort

	err := db.Order("host_id, sp_id").Find(&assignments).Error
	if err != nil {
		return nil, err
	}

	held := make(map[uint]bool, len(sps))
	for _, sp := range sps {
		held[sp.ID] = true
	}

	byHost := make(map[uint][]assignmentContent)

	for _, assignment := range assignments {
		if !held[assignment.SPID] {
			continue
		}

		createdAt := assignment.CreatedAt

		byHost[assignment.HostID] = append(byHost[assignment.HostID], assignmentContent{
			ServicePortID: assignment.SPID,
			BindScope:     assignment.BindScope,
			Enabled:       assignment.Enabled,
			CreatedAt:     &createdAt,
		})
	}

	return byHost, nil
}

// jumpsByHost reads the jump routes and returns the route of each Host id, in
// the order it is taken.
func jumpsByHost(db *gorm.DB) (map[uint][]uint, error) {
	var jumps []models.HostJump

	err := db.Order("host_id, seq").Find(&jumps).Error
	if err != nil {
		return nil, err
	}

	byHost := make(map[uint][]uint)

	for _, jump := range jumps {
		byHost[jump.HostID] = append(byHost[jump.HostID], jump.JumpHostID)
	}

	return byHost, nil
}

// localForwardsByHost reads the local forwards and returns them for each Host
// id, in the order of their number so that the same configuration gives the
// same file. A local forward whose Host is not among the ones exported is left
// out by the caller, since there is no Host to write it under.
func localForwardsByHost(db *gorm.DB) (map[uint][]localForwardContent, error) {
	var rows []models.LocalForward

	err := db.Order("host_id, number").Find(&rows).Error
	if err != nil {
		return nil, err
	}

	byHost := make(map[uint][]localForwardContent)

	for _, lf := range rows {
		enabled := lf.Enabled
		createdAt := lf.CreatedAt
		updatedAt := lf.UpdatedAt

		byHost[lf.HostID] = append(byHost[lf.HostID], localForwardContent{
			Number:         lf.Number,
			BindScope:      lf.BindScope,
			LocalPort:      lf.LocalPort,
			TargetAddress:  lf.TargetAddress,
			TargetPort:     lf.TargetPort,
			Description:    lf.Description,
			AllowedSources: lf.AllowedSources,
			Enabled:        &enabled,
			CreatedAt:      &createdAt,
			UpdatedAt:      &updatedAt,
		})
	}

	return byHost, nil
}

// ExportTunnels hands out every Host and every service port, sealed with the
// password in the body.
//
// @Summary      Every Host and every service port, encrypted into one file
// @Description  A POST and not a GET because the password that seals the file is in the body.
// @Description  Inside the file the SSH password, the private key and the key passphrase of every Host are in the clear, so treat it as the credentials of every Host it names.
// @Description  The file carries the id and the times of every Host and service port and the number of every local forward, so that an import puts the configuration back as it is here. Each Host carries its jump route in jump_host_ids, the service ports it carries in assignments, its local forwards, its pending host key and its SOCKS5 proxy. local_forwards in the answer counts the local forwards.
// @Tags         export and import
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.exportRequest  true  "The password that encrypts the file, and the password of the account"
// @Success  200  {object}  models.Response{data=api.exportedTunnels}
// @Failure  400  {object}  api.errorBody  "The password is refused, or account_password is empty"
// @Failure  401  {object}  api.errorBody  "account_password does not open this account"
// @Failure  429  {object}  api.errorBody  "Too many passwords that do not open this account were tried. Retry-After says when to try again"
// @Router       /export/tunnels [post]
func (h *TransferHandler) ExportTunnels(c echo.Context) error {
	var req exportRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
	}

	refused := h.accountPasswordRefused(c, req.AccountPassword, false, transferKindTunnels)
	if refused != nil {
		return refused.answer(c)
	}

	refused = checkExportPassword(req.Password)
	if refused != nil {
		return refused.answer(c)
	}

	var hosts []models.Host

	err = h.hosts.db.Order("id").Find(&hosts).Error
	if err != nil {
		h.hosts.logger.Error("failed to read the Hosts for an export", logid.TransferHostsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportHostsReadFailed)
	}

	var sps []models.ServicePort

	err = h.hosts.db.Order("id").Find(&sps).Error
	if err != nil {
		h.hosts.logger.Error("failed to read the service ports for an export",
			logid.TransferServicePortsReadFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportServicePortsRead)
	}

	assigned, err := assignedByHost(h.hosts.db, sps)
	if err != nil {
		h.hosts.logger.Error("failed to read the service port assignments for an export",
			logid.TransferAssignmentsReadFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportAssignmentsRead)
	}

	forwards, err := localForwardsByHost(h.hosts.db)
	if err != nil {
		h.hosts.logger.Error("failed to read the local forwards for an export",
			logid.TransferLocalForwardsReadFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportLocalForwardsRead)
	}

	jumps, err := jumpsByHost(h.hosts.db)
	if err != nil {
		h.hosts.logger.Error("failed to read the jump routes for an export",
			logid.TransferJumpsReadFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportJumpsRead)
	}

	exportedForwards := 0

	content := tunnelsContent{
		Hosts:        make([]hostContent, 0, len(hosts)),
		ServicePorts: make([]servicePortContent, 0, len(sps)),
	}

	for _, host := range hosts {
		opened, err := h.unsealHost(host)
		if err != nil {
			// A secret that does not open with the key in use is the key file
			// having been replaced or having come from another installation.
			// The export is stopped rather than made with that Host left out: a
			// file that quietly holds one Host fewer is one nobody checks.
			h.hosts.logger.Error("a stored secret of a Host does not open with the encryption key of "+
				"this installation, so no export was made",
				logid.TransferHostSecretDoesNotOpen.Field(), zap.Uint("host_id", host.ID),
				zap.Error(err))

			return failure(c, http.StatusInternalServerError, errExportHostSecretsSealed, errorArgs{"host": host.Address})
		}

		opened.Assignments = assigned[host.ID]
		opened.JumpHostIDs = jumps[host.ID]

		opened.LocalForwards = forwards[host.ID]
		if opened.LocalForwards == nil {
			opened.LocalForwards = []localForwardContent{}
		}

		exportedForwards += len(opened.LocalForwards)

		content.Hosts = append(content.Hosts, opened)
	}

	for _, sp := range sps {
		createdAt := sp.CreatedAt
		updatedAt := sp.UpdatedAt

		content.ServicePorts = append(content.ServicePorts, servicePortContent{
			ID:             sp.ID,
			ServiceAddress: sp.ServiceAddress,
			ServicePort:    sp.ServicePort,
			LocalPort:      sp.LocalPort,
			Description:    sp.Description,
			CreatedAt:      &createdAt,
			UpdatedAt:      &updatedAt,
		})
	}

	exportedAt := time.Now()

	sealed, err := h.seal(transferKindTunnels, content, req.Password, exportedAt)
	if err != nil {
		h.hosts.logger.Error("failed to encrypt the exported tunnel configuration",
			logid.TransferExportSealFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportSealFailed)
	}

	// What is logged is that an export was made and how much went into it. The
	// password and the file itself are not: the file is the credentials of
	// every Host, and the log is kept, rotated and read by more people than
	// hold the password.
	h.hosts.logger.Info("exported the tunnel configuration",
		logid.TransferExported.Field(),
		zap.Int("hosts", len(content.Hosts)),
		zap.Int("service_ports", len(content.ServicePorts)),
		zap.Int("local_forwards", exportedForwards))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: exportedTunnels{
			Kind:          transferKindTunnels,
			File:          sealed,
			ExportedAt:    exportedAt,
			Hosts:         len(content.Hosts),
			ServicePorts:  len(content.ServicePorts),
			LocalForwards: exportedForwards,
		},
	})
}

// ImportTunnels takes a file an export made and puts what it holds in place of
// the tunnel configuration stored here.
//
// Everything happens in one transaction, and anything that stops it rolls the
// whole of it back: a configuration that landed half way is one the operator
// has to take apart by hand before trying again.
//
// @Summary      Replace the tunnel configuration with what an exported tunnels file holds
// @Description  Every Host, service port, assignment, local forward and jump route stored here is deleted and the ones in the file are written in their place, under the ids and numbers the file carries. The account, the settings and the sessions are not touched.
// @Description  A file from before the ids were exported (format version 1) is written with ids counted from 1 in the order of the file, and with no jump route.
// @Description  dry_run true opens and checks the file and writes nothing. The answer is the same either way: current is what is stored here and file is what the file holds.
// @Description  overwrite is no longer read and a body that carries it is refused, so that a client written for the import that added and skipped rows does not replace the configuration without knowing it.
// @Description  The whole import is one transaction: a file that is refused leaves the database exactly as it was.
// @Description  The import is refused when a jump route of the file names a Host the file does not hold, the Host itself, one Host twice, or more than 8 Hosts, and when the file opens one local port twice, or opens the port this server listens on.
// @Tags         export and import
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.importTunnelsRequest  true  "The password, the file, whether to only check it, and the password of the account"
// @Success  200  {object}  models.Response{data=api.importedTunnels}
// @Failure  400  {object}  api.errorBody  "The password is wrong, the file is damaged, it is not a file this program wrote, it holds the other kind, a row of it is refused, the body carries overwrite, or account_password is empty"
// @Failure  401  {object}  api.errorBody  "account_password does not open this account"
// @Failure  429  {object}  api.errorBody  "Too many passwords that do not open this account were tried. Retry-After says when to try again"
// @Failure  409  {object}  api.errorBody  "A local forward or a SOCKS5 proxy of the file opens the port this server listens on. Nothing was stored"
// @Router       /import/tunnels [post]
func (h *TransferHandler) ImportTunnels(c echo.Context) error {
	overwrite, refused := bodyFieldKey(c, "overwrite")
	if refused != nil {
		return refused.answer(c)
	}

	if overwrite != "" {
		return failure(c, http.StatusBadRequest, errImportOverwriteRemoved)
	}

	var req importTunnelsRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
	}

	refused = h.accountPasswordRefused(c, req.AccountPassword, true, transferKindTunnels)
	if refused != nil {
		return refused.answer(c)
	}

	file, refused := h.open(req.File, req.Password, transferKindTunnels)
	if refused != nil {
		return refused.answer(c)
	}

	var content tunnelsContent

	err = json.Unmarshal(file.Content, &content)
	if err != nil {
		return failure(c, http.StatusBadRequest, errImportTunnelsUnreadable)
	}

	content.readOldNames()

	refused = checkLocalForwards(c, content)
	if refused != nil {
		return refused.answer(c)
	}

	refused = checkSocksPorts(content)
	if refused != nil {
		return refused.answer(c)
	}

	apiPort := 0

	if carriesLocalForwards(content) || opensSocks(content) {
		apiPort, err = h.hosts.storedAPIPort()
		if err != nil {
			h.hosts.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
		}
	}

	plan, refused := h.planImport(c, file.FormatVersion >= transferFormatWithIDs, content, apiPort)
	if refused != nil {
		return refused.answer(c)
	}

	if req.DryRun {
		current, err := storedCounts(h.hosts.db)
		if err != nil {
			h.hosts.logger.Error("failed to count what is stored for an import",
				logid.TransferHostsReadFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errImportCountsReadFailed)
		}

		return c.JSON(http.StatusOK, models.Response{
			Success: true,
			Data:    importedTunnels{DryRun: true, Current: current, File: plan.counts()},
		})
	}

	tx := h.hosts.db.Begin()

	err = tx.Error
	if err != nil {
		h.hosts.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

	current, err := storedCounts(tx)
	if err != nil {
		tx.Rollback()
		h.hosts.logger.Error("failed to count what is stored for an import",
			logid.TransferHostsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errImportCountsReadFailed)
	}

	refused = h.replaceConfiguration(tx, plan)
	if refused != nil {
		tx.Rollback()
		return refused.answer(c)
	}

	err = tx.Commit().Error
	if err != nil {
		h.hosts.logger.Error("failed to commit the transaction",
			logid.DatabaseTransactionCommitFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	// The loop is woken after the commit, for the reason every write handler
	// wakes it there: a pass that runs before it reads the rows as they were.
	h.hosts.manager.WakeReconcile()

	answer := importedTunnels{Current: current, File: plan.counts()}

	h.hosts.logger.Info("imported a tunnel configuration",
		logid.TransferImported.Field(),
		zap.Int("format_version", file.FormatVersion),
		zap.Int("removed_hosts", current.Hosts),
		zap.Int("hosts", answer.File.Hosts),
		zap.Int("service_ports", answer.File.ServicePorts),
		zap.Int("assignments", answer.File.Assignments),
		zap.Int("local_forwards", answer.File.LocalForwards),
		zap.Int("jump_hosts", answer.File.JumpHosts))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    answer,
	})
}

// importPlan is the rows a file is written as, built and checked in full before
// anything is deleted.
type importPlan struct {
	hosts         []models.Host
	servicePorts  []models.ServicePort
	assignments   []models.HostServicePort
	localForwards []models.LocalForward
	jumps         []models.HostJump
}

// counts is how much of each the plan writes.
func (plan importPlan) counts() transferCounts {
	routed := make(map[uint]bool)
	for _, jump := range plan.jumps {
		routed[jump.HostID] = true
	}

	return transferCounts{
		Hosts:         len(plan.hosts),
		ServicePorts:  len(plan.servicePorts),
		Assignments:   len(plan.assignments),
		LocalForwards: len(plan.localForwards),
		JumpHosts:     len(routed),
	}
}

// storedCounts is how much of each is stored.
func storedCounts(db *gorm.DB) (transferCounts, error) {
	var counts transferCounts

	for _, table := range []struct {
		model interface{}
		into  *int
	}{
		{&models.Host{}, &counts.Hosts},
		{&models.ServicePort{}, &counts.ServicePorts},
		{&models.HostServicePort{}, &counts.Assignments},
		{&models.LocalForward{}, &counts.LocalForwards},
	} {
		var rows int64

		err := db.Model(table.model).Count(&rows).Error
		if err != nil {
			return counts, err
		}

		*table.into = int(rows)
	}

	var routed int64

	err := db.Model(&models.HostJump{}).Distinct("host_id").Count(&routed).Error
	if err != nil {
		return counts, err
	}

	counts.JumpHosts = int(routed)

	return counts, nil
}

// timeOf is a time a file carries, or the zero time where it carries none, which
// gorm stores as the time of the write.
func timeOf(at *time.Time) time.Time {
	if at == nil {
		return time.Time{}
	}

	return *at
}

// planImport reads a file into the rows it is written as and holds every one of
// them to the rules, so that a file that is refused is refused before anything
// stored is deleted. withIDs is whether the file carries the ids of its rows; a
// file that does not is given ids counted from 1 in the order it lists them.
func (h *TransferHandler) planImport(c echo.Context, withIDs bool, content tunnelsContent,
	apiPort int) (importPlan, *refusal) {
	var plan importPlan

	hostIDs := make([]uint, len(content.Hosts))
	heldHostIDs := make(map[uint]bool, len(content.Hosts))
	heldAddresses := make(map[string]bool, len(content.Hosts))

	for i, host := range content.Hosts {
		id := uint(i + 1)
		if withIDs {
			id = host.ID
			if id == 0 || heldHostIDs[id] {
				return plan, refuse(http.StatusBadRequest, errImportHostIDInvalid,
					errorArgs{"host": host.Address, "id": strconv.FormatUint(uint64(host.ID), 10)})
			}
		}

		address := net.JoinHostPort(hostAddressKey(host.Address), strconv.Itoa(host.Port))
		if heldAddresses[address] {
			return plan, refuse(http.StatusBadRequest, errImportHostDuplicate,
				errorArgs{"host": host.Address, "port": strconv.Itoa(host.Port)})
		}

		heldHostIDs[id] = true
		heldAddresses[address] = true
		hostIDs[i] = id

		stored, refused := h.planHost(c, host, withIDs)
		if refused != nil {
			return plan, refused
		}

		stored.ID = id
		if withIDs {
			stored.PendingHostKey = host.PendingHostKey
			stored.CreatedAt = timeOf(host.CreatedAt)
			stored.UpdatedAt = timeOf(host.UpdatedAt)
		}

		if host.opensSocks() && isAPIPort(*host.SocksPort, apiPort, h.hosts.runningAPIPort) {
			return plan, refuse(http.StatusConflict, errImportSocksAPIPort,
				errorArgs{"host": host.Address, "socks_port": strconv.Itoa(*host.SocksPort)})
		}

		plan.hosts = append(plan.hosts, stored)
	}

	spIDOfLocalPort := make(map[int]uint, len(content.ServicePorts))
	heldSPIDs := make(map[uint]bool, len(content.ServicePorts))
	heldServices := make(map[string]bool, len(content.ServicePorts))

	for i, sp := range content.ServicePorts {
		serviceAddress := sp.ServiceAddress + ":" + strconv.Itoa(sp.ServicePort)
		named := transferText{textImportNameServicePort,
			textArgs{"service_address": serviceAddress, "local_port": strconv.Itoa(sp.LocalPort)}}
		name := named.english()
		nameCodes := map[string]textCode{"service_port": named.code}

		id := uint(i + 1)
		if withIDs {
			id = sp.ID
			if id == 0 || heldSPIDs[id] {
				return plan, refuse(http.StatusBadRequest, errImportServicePortIDInvalid,
					errorArgs{"service_port": name, "id": strconv.FormatUint(uint64(sp.ID), 10)}).
					named(nameCodes, named.values)
			}
		}

		err := c.Validate(&models.CreateServicePortRequest{
			ServiceAddress: sp.ServiceAddress,
			ServicePort:    sp.ServicePort,
			LocalPort:      sp.LocalPort,
			Description:    sp.Description,
		})
		if err != nil {
			return plan, refuse(http.StatusBadRequest, errImportServicePortRefused,
				errorArgs{"service_port": name, "reason": err.Error()}).named(nameCodes, named.values)
		}

		_, localPortHeld := spIDOfLocalPort[sp.LocalPort]
		if heldServices[serviceAddress] || localPortHeld {
			return plan, refuse(http.StatusBadRequest, errImportServicePortDuplicate,
				errorArgs{"service_port": name}).named(nameCodes, named.values)
		}

		heldSPIDs[id] = true
		heldServices[serviceAddress] = true
		spIDOfLocalPort[sp.LocalPort] = id

		stored := models.ServicePort{
			ID:             id,
			ServiceAddress: sp.ServiceAddress,
			ServicePort:    sp.ServicePort,
			LocalPort:      sp.LocalPort,
			Description:    sp.Description,
		}
		if withIDs {
			stored.CreatedAt = timeOf(sp.CreatedAt)
			stored.UpdatedAt = timeOf(sp.UpdatedAt)
		}

		plan.servicePorts = append(plan.servicePorts, stored)
	}

	for i, host := range content.Hosts {
		assignments, refused := planAssignments(withIDs, host, hostIDs[i], content, heldSPIDs, spIDOfLocalPort)
		if refused != nil {
			return plan, refused
		}

		plan.assignments = append(plan.assignments, assignments...)

		forwards, refused := h.planLocalForwards(withIDs, host, hostIDs[i], apiPort)
		if refused != nil {
			return plan, refused
		}

		plan.localForwards = append(plan.localForwards, forwards...)

		if !withIDs {
			continue
		}

		jumps, refused := planJumps(host, hostIDs[i], heldHostIDs)
		if refused != nil {
			return plan, refused
		}

		plan.jumps = append(plan.jumps, jumps...)
	}

	return plan, nil
}

// planHost holds one Host of the file to the rules of a create and seals its
// secrets with the key of this installation. A file with ids is stored with the
// SOCKS5 scope it carries, an empty one included, since it is the scope the
// row it was exported from held.
func (h *TransferHandler) planHost(c echo.Context, host hostContent, withIDs bool) (models.Host, *refusal) {
	name := host.Address

	// The rules of a create are run on what the file carries, so that a file
	// that was written by hand cannot put into the database what the screens
	// refuse: a port out of range, or something that is not an address.
	socks := host.socksOf(models.Host{})

	err := c.Validate(&models.CreateHostRequest{
		Address:             host.Address,
		Port:                host.Port,
		User:                host.User,
		Password:            host.Password,
		PrivateKey:          host.PrivateKey,
		KeyPassphrase:       host.KeyPassphrase,
		Description:         host.Description,
		SocksPort:           socks.SocksPort,
		SocksBindScope:      socks.SocksBindScope,
		SocksAllowedSources: socks.SocksAllowedSources,
	})
	if err != nil {
		return models.Host{}, refuse(http.StatusBadRequest, errImportHostRefused, errorArgs{"host": name, "reason": err.Error()})
	}

	// The rules a create holds the proxy to past the validator. The reason is
	// the English sentence of the refusal the Hosts screen would have given,
	// which names the Host the way every other refusal of a Host does.
	refusedSocks := checkSocks(&socks)
	if refusedSocks != nil {
		reason, _ := renderErrorMessage(errorMessages[refusedSocks.code], refusedSocks.args)
		return models.Host{}, refuse(http.StatusBadRequest, errImportHostRefused, errorArgs{"host": name, "reason": reason})
	}

	// The scopes the file names are held to the two words as well. The column
	// is under the same rule in the database, and a third word would otherwise
	// be met as a failed write halfway through the import, which names the row
	// and not what is wrong with it.
	unknown := unknownBindScope(host.AssignedBindScopes)
	if unknown == "" {
		unknown = unknownAssignmentScope(host.Assignments)
	}
	if unknown != "" {
		return models.Host{}, refuse(http.StatusBadRequest, errImportHostRefused, errorArgs{"host": name, "reason": unknown})
	}

	if host.Password == "" && strings.TrimSpace(host.PrivateKey) == "" {
		return models.Host{}, refuse(http.StatusBadRequest, errImportHostNoLogin, errorArgs{"host": name})
	}

	// Sealed with the key of this installation, which is what makes the file
	// work across installations: the secrets came in as plaintext and are
	// stored here the way a create on this machine stores them.
	password, err := h.hosts.sealPassword(host.Password)
	if err != nil {
		h.hosts.logger.Error("failed to encrypt the password of an imported Host",
			logid.TransferHostPasswordEncryptFailed.Field(),
			zap.Error(err))
		return models.Host{}, refuse(http.StatusInternalServerError, errImportHostPasswordEncrypt, errorArgs{"host": name})
	}

	privateKey, keyPassphrase, err := h.hosts.sealPrivateKey(host.PrivateKey, host.KeyPassphrase)
	if err != nil {
		var refusedKey *tunnel.KeyError
		if errors.As(err, &refusedKey) {
			return models.Host{}, refuse(http.StatusBadRequest, errImportHostKeyRefused, errorArgs{"host": name, "reason": refusedKey.Error()})
		}

		h.hosts.logger.Error("failed to encrypt the private key of an imported Host",
			logid.TransferHostPrivateKeyEncryptFailed.Field(),
			zap.Error(err))

		return models.Host{}, refuse(http.StatusInternalServerError, errImportHostKeyEncrypt, errorArgs{"host": name})
	}

	stored := models.Host{
		Address:       host.Address,
		Port:          host.Port,
		User:          host.User,
		Password:      password,
		PrivateKey:    privateKey,
		KeyPassphrase: keyPassphrase,
		HostKey:       host.HostKey,
		Description:   host.Description,
		Enabled:       host.Enabled,
	}

	stored = host.socksOf(stored)
	if withIDs && host.SocksBindScope != nil {
		stored.SocksBindScope = *host.SocksBindScope
	}

	return stored, nil
}

// unknownAssignmentScope is unknownBindScope for the assignments of a file that
// names them by the id of the service port.
func unknownAssignmentScope(assignments []assignmentContent) string {
	for _, assignment := range assignments {
		switch assignment.BindScope {
		case "", models.BindScopeLoopback, models.BindScopeWildcard:
			continue
		}

		return "the assignment of the service port " + strconv.FormatUint(uint64(assignment.ServicePortID), 10) +
			" is opened to " + assignment.BindScope + ", which is neither " + models.BindScopeLoopback + " nor " +
			models.BindScopeWildcard
	}

	return ""
}

// planAssignments is the service ports one Host of the file carries.
//
// A file with ids names them by the id of the service port. A file without
// names them by the local port, and one that does not name them at all was
// written before they were stored, when every Host carried every service port.
// Either way a service port the file does not hold is refused: every service
// port stored here after the import is one of the file.
func planAssignments(withIDs bool, host hostContent, hostID uint, content tunnelsContent,
	heldSPIDs map[uint]bool, spIDOfLocalPort map[int]uint) ([]models.HostServicePort, *refusal) {
	assignments := make([]models.HostServicePort, 0)
	assigned := make(map[uint]bool)

	if withIDs {
		for _, carried := range host.Assignments {
			if !heldSPIDs[carried.ServicePortID] {
				return nil, refuse(http.StatusBadRequest, errImportAssignmentUnknownServicePort,
					errorArgs{"host": host.Address,
						"service_port_id": strconv.FormatUint(uint64(carried.ServicePortID), 10)})
			}

			if assigned[carried.ServicePortID] {
				continue
			}

			assigned[carried.ServicePortID] = true

			assignments = append(assignments, models.HostServicePort{
				HostID:    hostID,
				SPID:      carried.ServicePortID,
				BindScope: carried.BindScope,
				Enabled:   carried.Enabled,
				CreatedAt: timeOf(carried.CreatedAt),
			})
		}

		return assignments, nil
	}

	wanted := host.AssignedLocalPorts
	if wanted == nil {
		wanted = make([]int, 0, len(content.ServicePorts))
		for _, sp := range content.ServicePorts {
			wanted = append(wanted, sp.LocalPort)
		}
	}

	for _, localPort := range wanted {
		spID, held := spIDOfLocalPort[localPort]
		if !held {
			return nil, refuse(http.StatusBadRequest, errImportAssignmentUnknownLocalPort,
				errorArgs{"host": host.Address, "local_port": strconv.Itoa(localPort)})
		}

		if assigned[spID] {
			continue
		}

		assigned[spID] = true

		// Each assignment is written on the scope the file gives it, which a
		// file from before the scopes were stored answers with the bind
		// address it carries for the whole Host.
		assignments = append(assignments, models.HostServicePort{
			HostID:    hostID,
			SPID:      spID,
			BindScope: bindScopeOf(host, localPort),
			Enabled:   enabledOf(host, localPort),
		})
	}

	return assignments, nil
}

// planLocalForwards is the local forwards one Host of the file carries. A file
// without ids numbers them from 1 in the order it lists them.
func (h *TransferHandler) planLocalForwards(withIDs bool, host hostContent, hostID uint,
	apiPort int) ([]models.LocalForward, *refusal) {
	forwards := make([]models.LocalForward, 0, len(host.LocalForwards))
	numbered := make(map[uint]bool, len(host.LocalForwards))

	for i, lf := range host.LocalForwards {
		localPort := strconv.Itoa(lf.LocalPort)

		number := uint(i + 1)
		if withIDs {
			number = lf.Number
			if number == 0 || numbered[number] {
				return nil, refuse(http.StatusBadRequest, errImportLocalForwardNumberInvalid,
					errorArgs{"host": host.Address, "local_port": localPort,
						"number": strconv.FormatUint(uint64(lf.Number), 10)})
			}
		}

		numbered[number] = true

		if isAPIPort(lf.LocalPort, apiPort, h.hosts.runningAPIPort) {
			return nil, refuse(http.StatusConflict, errImportLocalForwardAPIPort,
				errorArgs{"host": host.Address, "local_port": localPort})
		}

		// An empty scope of a file without ids is stored as the word it
		// stands for, the way a create stores it. A file with ids is stored
		// as the row it was exported from held it.
		bindScope := lf.BindScope
		if bindScope == "" && !withIDs {
			bindScope = models.BindScopeWildcard
		}

		stored := models.LocalForward{
			HostID:         hostID,
			Number:         number,
			BindScope:      bindScope,
			LocalPort:      lf.LocalPort,
			TargetAddress:  lf.TargetAddress,
			TargetPort:     lf.TargetPort,
			Description:    lf.Description,
			AllowedSources: lf.AllowedSources,
			Enabled:        lf.enabled(),
		}
		if withIDs {
			stored.CreatedAt = timeOf(lf.CreatedAt)
			stored.UpdatedAt = timeOf(lf.UpdatedAt)
		}

		forwards = append(forwards, stored)
	}

	return forwards, nil
}

// planJumps is the jump route of one Host of the file, held to naming Hosts of
// the same file, never the Host itself, and none of them twice.
func planJumps(host hostContent, hostID uint, heldHostIDs map[uint]bool) ([]models.HostJump, *refusal) {
	if len(host.JumpHostIDs) > maxJumpHosts {
		return nil, refuse(http.StatusBadRequest, errImportJumpTooMany,
			errorArgs{"host": host.Address, "max": strconv.Itoa(maxJumpHosts)})
	}

	jumps := make([]models.HostJump, 0, len(host.JumpHostIDs))
	passed := make(map[uint]bool, len(host.JumpHostIDs))

	for i, jumpHostID := range host.JumpHostIDs {
		jumpHost := strconv.FormatUint(uint64(jumpHostID), 10)

		switch {
		case jumpHostID == hostID:
			return nil, refuse(http.StatusBadRequest, errImportJumpSelf, errorArgs{"host": host.Address})
		case !heldHostIDs[jumpHostID]:
			return nil, refuse(http.StatusBadRequest, errImportJumpUnknownHost,
				errorArgs{"host": host.Address, "jump_host_id": jumpHost})
		case passed[jumpHostID]:
			return nil, refuse(http.StatusBadRequest, errImportJumpDuplicate,
				errorArgs{"host": host.Address, "jump_host_id": jumpHost})
		}

		passed[jumpHostID] = true

		jumps = append(jumps, models.HostJump{HostID: hostID, Seq: uint(i + 1), JumpHostID: jumpHostID})
	}

	return jumps, nil
}

// replaceConfiguration deletes the tunnel configuration stored here and writes
// the plan in its place, inside the transaction it is handed.
//
// The rows of the tunnels are the state of the connections this process holds,
// which the reconcile loop keeps. The ones whose assignment the plan still
// carries are left to it: it restarts a tunnel whose Host or service port is
// now another, and one that is the same goes on running on the row it writes
// its state to. The rest are deleted with the configuration they stood for.
func (h *TransferHandler) replaceConfiguration(tx *gorm.DB, plan importPlan) *refusal {
	for _, model := range []interface{}{
		&models.HostJump{},
		&models.LocalForward{},
		&models.HostServicePort{},
		&models.ServicePort{},
		&models.Host{},
	} {
		err := tx.Where("1 = 1").Delete(model).Error
		if err != nil {
			h.hosts.logger.Error("failed to delete the tunnel configuration while importing",
				logid.TransferConfigurationClearFailed.Field(), zap.Error(err))
			return refuse(http.StatusInternalServerError, errImportClearFailed)
		}
	}

	for i := range plan.hosts {
		err := tx.Create(&plan.hosts[i]).Error
		if err != nil {
			h.hosts.logger.Error("failed to create a Host while importing",
				logid.TransferHostCreateFailed.Field(),
				zap.Error(err))
			return refuse(http.StatusInternalServerError, errImportHostCreateFailed, errorArgs{"host": plan.hosts[i].Address})
		}
	}

	for i := range plan.servicePorts {
		err := tx.Create(&plan.servicePorts[i]).Error
		if err != nil {
			sp := plan.servicePorts[i]
			named := transferText{textImportNameServicePort, textArgs{
				"service_address": sp.ServiceAddress + ":" + strconv.Itoa(sp.ServicePort),
				"local_port":      strconv.Itoa(sp.LocalPort)}}

			h.hosts.logger.Error("failed to create a service port while importing",
				logid.TransferServicePortCreateFailed.Field(),
				zap.Error(err))
			return refuse(http.StatusInternalServerError, errImportServicePortCreate,
				errorArgs{"service_port": named.english()}).
				named(map[string]textCode{"service_port": named.code}, named.values)
		}
	}

	hostOf := make(map[uint]string, len(plan.hosts))
	for _, host := range plan.hosts {
		hostOf[host.ID] = host.Address
	}

	for i := range plan.assignments {
		err := tx.Create(&plan.assignments[i]).Error
		if err != nil {
			h.hosts.logger.Error("failed to store an assignment while importing",
				logid.TransferAssignmentStoreFailed.Field(),
				zap.Error(err))
			return refuse(http.StatusInternalServerError, errImportAssignmentsStoreFailed,
				errorArgs{"host": hostOf[plan.assignments[i].HostID]})
		}
	}

	for i := range plan.localForwards {
		err := tx.Create(&plan.localForwards[i]).Error
		if err != nil {
			h.hosts.logger.Error("failed to store a local forward while importing",
				logid.TransferLocalForwardStoreFailed.Field(),
				zap.Error(err))
			return refuse(http.StatusInternalServerError, errImportLocalForwardsStoreFailed,
				errorArgs{"host": hostOf[plan.localForwards[i].HostID]})
		}
	}

	for i := range plan.jumps {
		err := tx.Create(&plan.jumps[i]).Error
		if err != nil {
			h.hosts.logger.Error("failed to store a jump route while importing",
				logid.TransferJumpStoreFailed.Field(),
				zap.Error(err))
			return refuse(http.StatusInternalServerError, errImportJumpsStoreFailed,
				errorArgs{"host": hostOf[plan.jumps[i].HostID]})
		}
	}

	err := tx.Where("NOT EXISTS (SELECT 1 FROM host_service_ports WHERE " +
		"host_service_ports.host_id = tunnels.host_id AND host_service_ports.sp_id = tunnels.sp_id)").
		Delete(&models.Tunnel{}).Error
	if err != nil {
		h.hosts.logger.Error("failed to delete the tunnel configuration while importing",
			logid.TransferConfigurationClearFailed.Field(), zap.Error(err))
		return refuse(http.StatusInternalServerError, errImportClearFailed)
	}

	return nil
}

// unknownBindScope names the first entry of a file that is not a scope an
// assignment may be on, and is empty when every one of them is.
//
// The keys are walked in order, so that a file with several of them is refused
// with the same one named on every run: a map is walked in whatever order it
// happens to be in, and a refusal that names a different entry each time reads
// as more than one fault.
func unknownBindScope(scopes map[string]string) string {
	localPorts := make([]string, 0, len(scopes))
	for localPort := range scopes {
		localPorts = append(localPorts, localPort)
	}

	sort.Strings(localPorts)

	for _, localPort := range localPorts {
		switch scopes[localPort] {
		case "", models.BindScopeLoopback, models.BindScopeWildcard:
			continue
		}

		return "the assignment on the local port " + localPort + " is opened to " +
			scopes[localPort] + ", which is neither " + models.BindScopeLoopback + " nor " +
			models.BindScopeWildcard
	}

	return ""
}

// bindScopeOf is how far one assignment of a file reaches.
//
// What the file names under that local port is the answer. A file that names
// none falls back to the bind address of the Host, which is what a file from
// the release that kept one answer for the whole Host carries: a loopback
// address there means every assignment of that Host was on loopback, and every
// other answer, an address of some interface of the machine among them, means
// the wildcard. That is what database.fillBindScopes makes of the same column
// on an upgrade, and a file from that release has to say what the database of
// it said.
//
// A file this version wrote carries no bind address at all, so the fallback is
// the empty value there, which is the wildcard.
func bindScopeOf(host hostContent, localPort int) string {
	scope, named := host.AssignedBindScopes[strconv.Itoa(localPort)]
	if named {
		return scope
	}

	address := net.ParseIP(host.BindAddress)
	if address != nil && address.IsLoopback() {
		return models.BindScopeLoopback
	}

	return ""
}

// enabledOf is whether the assignment of one local port runs, as the file says.
// An assignment the file says nothing about runs, which is what every
// assignment of a file from before they could be switched off was doing.
func enabledOf(host hostContent, localPort int) bool {
	enabled, named := host.AssignedEnabled[strconv.Itoa(localPort)]
	if named {
		return enabled
	}

	return true
}

// carriesLocalForwards reports whether any Host of a file carries a local
// forward.
func carriesLocalForwards(content tunnelsContent) bool {
	for _, host := range content.Hosts {
		if len(host.LocalForwards) > 0 {
			return true
		}
	}

	return false
}

// checkLocalForwards holds every local forward of a file to the rules of a
// create, and refuses a file that opens one local port twice. It runs before
// anything is written, because two rows of the file meeting each other is a
// fault of the file wherever in it they are.
func checkLocalForwards(c echo.Context, content tunnelsContent) *refusal {
	openedBy := make(map[int]bool)

	for _, host := range content.Hosts {
		for _, lf := range host.LocalForwards {
			localPort := strconv.Itoa(lf.LocalPort)

			err := c.Validate(&models.LocalForwardRequest{
				BindScope:     lf.BindScope,
				LocalPort:     lf.LocalPort,
				TargetAddress: lf.TargetAddress,
				TargetPort:    lf.TargetPort,
				Description:   lf.Description,
			})
			if err == nil {
				_, err = tunnel.ParseAllowedSources(lf.AllowedSources)
			}
			if err != nil {
				return refuse(http.StatusBadRequest, errImportLocalForwardRefused,
					errorArgs{"host": host.Address, "local_port": localPort, "reason": err.Error()})
			}

			if openedBy[lf.LocalPort] {
				return refuse(http.StatusBadRequest, errImportLocalForwardDuplicate, errorArgs{"local_port": localPort})
			}

			openedBy[lf.LocalPort] = true
		}
	}

	return nil
}

// opensSocks reports whether any Host of a file switches its SOCKS5 proxy on.
func opensSocks(content tunnelsContent) bool {
	for _, host := range content.Hosts {
		if host.opensSocks() {
			return true
		}
	}

	return false
}

// checkSocksPorts refuses a file that opens one port with two SOCKS5 proxies,
// or with a SOCKS5 proxy and a local forward. It runs before anything is
// written for the reason checkLocalForwards does, after it, so that two local
// forwards meeting each other are answered as they always were.
func checkSocksPorts(content tunnelsContent) *refusal {
	openedBy := make(map[int]bool)

	for _, host := range content.Hosts {
		for _, lf := range host.LocalForwards {
			openedBy[lf.LocalPort] = true
		}
	}

	for _, host := range content.Hosts {
		if !host.opensSocks() {
			continue
		}

		if openedBy[*host.SocksPort] {
			return refuse(http.StatusBadRequest, errImportSocksDuplicate,
				errorArgs{"port": strconv.Itoa(*host.SocksPort)})
		}

		openedBy[*host.SocksPort] = true
	}

	return nil
}

// ExportSettings hands out the stored settings, sealed with the password in the
// body.
//
// The settings hold no secret of their own, and the file is sealed all the
// same: it is one format, one password and one thing to explain, and a second
// format that happens not to need a password today would be the one somebody
// puts a secret into tomorrow.
//
// @Summary      The stored settings, encrypted into one file
// @Description  A POST and not a GET because the password that seals the file is in the body.
// @Tags         export and import
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.exportRequest  true  "The password that encrypts the file, and the password of the account"
// @Success  200  {object}  models.Response{data=api.exportedSettings}
// @Failure  400  {object}  api.errorBody  "The password is refused, or account_password is empty"
// @Failure  401  {object}  api.errorBody  "account_password does not open this account"
// @Failure  429  {object}  api.errorBody  "Too many passwords that do not open this account were tried. Retry-After says when to try again"
// @Router       /export/settings [post]
func (h *TransferHandler) ExportSettings(c echo.Context) error {
	var req exportRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
	}

	refused := h.accountPasswordRefused(c, req.AccountPassword, false, transferKindSettings)
	if refused != nil {
		return refused.answer(c)
	}

	refused = checkExportPassword(req.Password)
	if refused != nil {
		return refused.answer(c)
	}

	// The webhook address and the mail settings are opened here and carried
	// in the clear, for the reason the password of the mail server is below.
	stored, err := settings.LoadOpened(h.hosts.db, h.hosts.cipher)
	if errors.Is(err, settings.ErrAlertSecretsDoNotOpen) {
		h.hosts.logger.Error("a stored secret of the settings does not open with the encryption key in use",
			logid.TransferSettingsSecretDoesNotOpen.Field(),
			zap.String("setting", "alert"),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportSealFailed)
	}
	if err != nil {
		h.hosts.logger.Error("failed to read the settings for an export",
			logid.TransferSettingsReadForExportFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	content := settingsOf(stored)

	// The password of the mail server is opened here for the reason the
	// passwords of the Hosts are opened in unsealHost.
	password, err := h.unseal(stored.SMTPPassword)
	if err != nil {
		h.hosts.logger.Error("a stored secret of the settings does not open with the encryption key in use",
			logid.TransferSettingsSecretDoesNotOpen.Field(),
			zap.String("setting", smtpPasswordSetting),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportSealFailed)
	}

	content.SMTPPassword = &password

	exportedAt := time.Now()

	sealed, err := h.seal(transferKindSettings, content, req.Password, exportedAt)
	if err != nil {
		h.hosts.logger.Error("failed to encrypt the exported settings", logid.TransferSettingsSealFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportSealFailed)
	}

	h.hosts.logger.Info("exported the settings of the manager", logid.TransferSettingsExported.Field())

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: exportedSettings{
			Kind:       transferKindSettings,
			File:       sealed,
			ExportedAt: exportedAt,
		},
	})
}

// ImportSettings stores the settings a file carries.
//
// It stores them and nothing else: no setting is put onto the running process,
// not even the one a save on the Settings screen puts into place. A file that
// arrives from another installation changes the port that is being listened on,
// the interval of the loops and where the logs go, and putting those onto a
// process in the middle of answering the very request that carries them is how
// an import ends with nobody able to reach the server. What is stored is what
// the next startup runs on, and GET /api/settings reports the difference in
// pending_restart until then.
//
// logging.level is the one setting that is neither put into place nor reported
// as waiting, because a read leaves out of pending_restart the settings a save
// normally applies at once. It takes hold at the next restart like the rest.
//
// @Summary      Store the settings an exported settings file holds
// @Description  It stores them and puts none of them onto the running process, api_port and api_https_enabled included. GET /api/settings reports the difference in pending_restart until the next startup.
// @Description  A new api_port that a local forward opens as its local port is refused with 409 and nothing is stored; data then carries that local forward and suggested_port, as PUT /settings does.
// @Description  A new api_port that the SOCKS5 proxy of a Host opens is refused the same way under its own error_code, with that Host in socks_host.
// @Tags         export and import
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.importRequest  true  "The password and the file"
// @Success  200  {object}  models.Response{data=api.importedSettings}
// @Failure  400  {object}  api.errorBody  "The file does not open, or its settings do not pass the rules of the Settings screen"
// @Failure  409  {object}  api.errorBody{data=api.apiPortTaken}  "The api_port of the file is the local port of a local forward or the port of a SOCKS5 proxy. Nothing was stored"
// @Router       /import/settings [post]
func (h *TransferHandler) ImportSettings(c echo.Context) error {
	var req importRequest

	err := c.Bind(&req)
	if err != nil {
		return unreadableBody(err).answer(c)
	}

	file, refused := h.open(req.File, req.Password, transferKindSettings)
	if refused != nil {
		return refused.answer(c)
	}

	// Opened so that a file that leaves the webhook address and the mail
	// settings out keeps them as they are stored, and so that they are held
	// against what the file names in the clear.
	stored, err := settings.LoadOpened(h.hosts.db, h.hosts.cipher)
	if err != nil {
		h.hosts.logger.Error("failed to read the settings for an import",
			logid.TransferSettingsReadForImportFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	before := *stored

	// The content is read onto the settings that are stored, so that a file
	// that does not name a setting leaves that setting as it is. Read onto an
	// empty set instead, every setting the file left out would arrive as a zero
	// value and be stored as one or refused as one.
	content := settingsOf(stored)

	err = json.Unmarshal(file.Content, &content)
	if err != nil {
		return failure(c, http.StatusBadRequest, errImportSettingsUnreadable)
	}

	updated := *stored
	content.applyTo(&updated)

	// Before the rules are run, because the two paths are the one thing in the
	// file this installation repairs rather than refuses: see
	// dropPathsOutsideTheInstallation.
	dropped := dropPathsOutsideTheInstallation(&updated)

	// The rules are run here as well as inside Save, so that a value they
	// refuse is answered as a bad request while a database that could not be
	// written to stays a 500. It is the same split UpdateSettings makes.
	err = updated.Validate()
	if err != nil {
		return failure(c, http.StatusBadRequest, errImportSettingsRefused, errorArgs{"reason": err.Error()})
	}

	// Refused rather than dropped to the default like a path outside the
	// installation: a file that names the key as the log was not written by
	// an installation that ran on it, so there is no older rule it followed.
	err = updated.ValidateFiles(h.databaseFile)
	if err != nil {
		return failure(c, http.StatusBadRequest, errImportSettingsRefused, errorArgs{"reason": err.Error()})
	}

	// The password the file carries is sealed with the key of this
	// installation, the way an imported Host has its password sealed.
	if content.SMTPPassword != nil {
		updated.SMTPPassword = ""

		if *content.SMTPPassword != "" {
			sealed, err := h.hosts.cipher.Encrypt(*content.SMTPPassword)
			if err != nil {
				h.hosts.logger.Error("failed to encrypt a secret of the imported settings",
					logid.TransferSettingsSecretSealFailed.Field(),
					zap.String("setting", smtpPasswordSetting),
					zap.Error(err))
				return failure(c, http.StatusInternalServerError, errSettingsSMTPPasswordSeal)
			}

			updated.SMTPPassword = sealed
		}
	} else if mailTargetMoved(&before, &updated) {
		// A file that names another mail target and no password drops the
		// stored one rather than keeping it. Kept, it would be sent to a
		// server the file chose, which is what guardStoredPassword stops a
		// save from doing. The import is not refused over it, because the
		// rest of the file is what the operator asked for, and the password
		// is the one setting that has to be typed again either way.
		updated.SMTPPassword = ""
	}

	// The settings were read above, before the transaction, for the reason
	// storedAPIPort gives: the pool holds one connection.
	tx := h.hosts.db.Begin()

	err = tx.Error
	if err != nil {
		h.hosts.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}
	defer rollbackUnlessDone(tx)

	// Held to the local forwards the way a save on the Settings screen is, and
	// only when the port changes, for the same reason.
	if updated.APIPort != before.APIPort {
		refused, err := apiPortRefused(tx, apiPortCodes{errImportSettingsAPIPortForward, errImportSettingsAPIPortSocks}, updated.APIPort, before.APIPort,
			h.hosts.runningAPIPort)
		if err != nil {
			tx.Rollback()
			h.hosts.logger.Error("failed to look for a local forward while importing",
				logid.TransferLocalForwardLookupFailed.Field(),
				zap.Error(err))
			return failure(c, http.StatusInternalServerError, errImportLocalForwardsReadFailed)
		}
		if refused != nil {
			tx.Rollback()
			return refused.answer(c)
		}
	}

	err = settings.Save(tx, &updated, h.hosts.cipher)
	if err != nil {
		tx.Rollback()
		h.hosts.logger.Error("failed to store the imported settings",
			logid.TransferSettingsStoreFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsStoreFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.hosts.logger.Error("failed to commit the transaction",
			logid.DatabaseTransactionCommitFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	diff := settings.Diff(&before, &updated)

	// The list is built even when it is empty, so that an import that changed
	// nothing answers with an empty list rather than with a null the screen
	// would have to tell from a list it failed to read.
	changes := make([]settingsChange, 0, len(diff))

	for _, change := range diff {
		changes = append(changes, settingsChange{
			Name:    change.Name,
			From:    change.From,
			To:      change.To,
			Applied: appliedRestart,
		})
	}

	// A dropped path is written under the id the startup writes it under, with
	// the same two halves: what the file asked for is gone once this has been
	// stored, and it is the only place it is written down. The line is the
	// warning it is there too, because the installation is now reading its
	// encryption key somewhere other than the one the file named.
	for _, change := range dropped {
		h.hosts.logger.Warn("a path setting of an imported file names a place outside the directory "+
			"the database file is in, which is not allowed, and was stored as its default",
			logid.SettingsSettingPutBackToDefault.Field(),
			zap.String("setting", change.Name),
			zap.String("from", change.From),
			zap.String("to", change.To))
	}

	h.hosts.logger.Info("imported the settings of the manager",
		logid.TransferSettingsImported.Field(),
		zap.Int("changed", len(changes)))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: importedSettings{
			Settings:        &updated,
			Changes:         changes,
			Items:           droppedPathItems(dropped),
			RestartRequired: len(changes) > 0,
		},
	})
}
