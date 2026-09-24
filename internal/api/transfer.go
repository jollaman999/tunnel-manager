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
const transferFormatVersion = 1

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
// The id and the two timestamps are left out. They describe the rows of the
// installation that was exported, and the import writes rows of its own.
type hostContent struct {
	IP            string `json:"ip"`
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
	// models.Host.PendingHostKey is deliberately not here. It is what some
	// server presented on a connection that was refused, which is the state of
	// a connection this installation made rather than anything the operator
	// asked for, and the question it stands for is about a machine that the
	// installation reading the file has not spoken to yet. Carried across, it
	// would put an approval on the screen of the other installation for a key
	// nothing there ever saw. The import drops it for the same reason:
	// importHost.
	HostKey string `json:"host_key"`
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
	AssignedLocalPorts []int `json:"assigned_local_ports"`
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
	// LocalForwards are the local forwards this Host carries, in the order of
	// their local port. nil and an empty list are told apart the way they are
	// for AssignedLocalPorts:
	//
	//	no field, or null  the file says nothing of them; they are left as they are
	//	[]                 this Host forwards nothing
	//
	// A file from before they were stored carries no field, and it says nothing
	// about the local forwards made here, so an overwrite does not clear them.
	// The export therefore never writes nil.
	LocalForwards []localForwardContent `json:"local_forwards"`
}

// localForwardContent is one local forward as it is carried in a file, on the
// Host it belongs to. The id, the Host id and the timestamps are left out for
// the reason they are left out of a Host.
type localForwardContent struct {
	BindScope   string `json:"bind_scope"`
	LocalPort   int    `json:"local_port"`
	TargetIP    string `json:"target_ip"`
	TargetPort  int    `json:"target_port"`
	Description string `json:"description"`
}

// servicePortContent is one service port as it is carried in a file. It holds
// no secret, and the id and the timestamps are left out for the reason they are
// left out of a Host.
type servicePortContent struct {
	ServiceIP   string `json:"service_ip"`
	ServicePort int    `json:"service_port"`
	LocalPort   int    `json:"local_port"`
	Description string `json:"description"`
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
	}
}

// applyTo puts what the file carries onto a set of settings, leaving the id and
// the timestamp of that set alone.
func (content *settingsContent) applyTo(s *settings.Settings) {
	s.APIPort = content.APIPort
	s.APIHTTPSEnabled = content.APIHTTPSEnabled
	s.MonitoringIntervalSec = content.MonitoringIntervalSec
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
}

// exportRequest is what an export is asked for. The password seals the file and
// is the only thing that opens it again: it is not stored anywhere, so a
// forgotten one leaves the file unreadable.
type exportRequest struct {
	Password string `json:"password"`
}

// importRequest is what an import is given: the file as the export handed it
// out, the password it was sealed with, and what to do about a row that is
// already there.
type importRequest struct {
	Password  string `json:"password"`
	File      string `json:"file"`
	Overwrite bool   `json:"overwrite"`
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

// What became of one row an import found in the file. A row that was there
// already is told from one that was written, because that is what says whether
// running the import again with overwrite on would change anything.
const (
	transferAdded    = "added"
	transferReplaced = "replaced"
	transferSkipped  = "skipped"
)

// transferItem is one row of the file and what the import did with it. The name
// is how the operator finds the row on the screens: the IP of a Host, and the
// service and local port of a service port.
type transferItem struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// importedTunnels is the answer to an import of the tunnel configuration. Every
// row of the file is in the list, the ones that were skipped included, along
// with why it was skipped: the operator reads that and decides whether to send
// the same file again with overwrite on.
type importedTunnels struct {
	Items    []transferItem `json:"items"`
	Added    int            `json:"added"`
	Replaced int            `json:"replaced"`
	Skipped  int            `json:"skipped"`
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
		carried := change.From
		if carried == "" {
			carried = "an empty path"
		}

		items = append(items, transferItem{
			Kind:   "setting",
			Name:   change.Name,
			Action: transferSkipped,
			Reason: "the file carries " + carried + ", which does not name a file under the " +
				"directory the database file is in, so " + change.To + " was stored instead",
		})
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
}

func NewTransferHandler(hosts *Handler, version string) *TransferHandler {
	return &TransferHandler{
		hosts:   hosts,
		version: version,
	}
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

	return hostContent{
		IP:            host.IP,
		Port:          host.Port,
		User:          host.User,
		Password:      password,
		PrivateKey:    privateKey,
		KeyPassphrase: keyPassphrase,
		HostKey:       host.HostKey,
		Description:   host.Description,
		Enabled:       host.Enabled,
	}, nil
}

// hostAssignments is what one Host carries as a file says it: the local ports
// of the service ports assigned to it, and how far each of those assignments
// reaches, keyed by the local port written as text.
type hostAssignments struct {
	localPorts []int
	bindScopes map[string]string
}

// assignedByHost reads the assignments and returns that for each Host id. The
// service ports are handed in rather than read again, since the export has them
// already and the two reads have to agree on what is stored.
//
// An assignment whose service port is not among them is left out. It points at
// a row that is not there, so there is no local port to write it as, and an id
// carried across would name a different service port at the other installation.
//
// The lists are sorted, so that exporting the same configuration twice gives
// the same file rather than whatever order the rows came back in. The scopes
// need no sorting: encoding/json writes the keys of a map in order.
//
// A scope that is the empty value gets no entry. It is the wildcard, which is
// what an assignment with nothing said about it is on at the other end too, so
// the file says only what was chosen.
func assignedByHost(db *gorm.DB, sps []models.ServicePort) (map[uint]hostAssignments, error) {
	var assignments []models.HostServicePort

	err := db.Find(&assignments).Error
	if err != nil {
		return nil, err
	}

	localPortOf := make(map[uint]int, len(sps))
	for _, sp := range sps {
		localPortOf[sp.ID] = sp.LocalPort
	}

	byHost := make(map[uint]hostAssignments)

	for _, assignment := range assignments {
		localPort, known := localPortOf[assignment.SPID]
		if !known {
			continue
		}

		carried := byHost[assignment.HostID]
		carried.localPorts = append(carried.localPorts, localPort)

		if assignment.BindScope != "" {
			if carried.bindScopes == nil {
				carried.bindScopes = make(map[string]string)
			}

			carried.bindScopes[strconv.Itoa(localPort)] = assignment.BindScope
		}

		byHost[assignment.HostID] = carried
	}

	for hostID, carried := range byHost {
		sort.Ints(carried.localPorts)
		byHost[hostID] = carried
	}

	return byHost, nil
}

// localForwardsByHost reads the local forwards and returns them for each Host
// id, in the order of their local port so that the same configuration gives the
// same file. A local forward whose Host is not among the ones exported is left
// out by the caller, since there is no Host to write it under.
func localForwardsByHost(db *gorm.DB) (map[uint][]localForwardContent, error) {
	var rows []models.LocalForward

	err := db.Order("local_port").Find(&rows).Error
	if err != nil {
		return nil, err
	}

	byHost := make(map[uint][]localForwardContent)

	for _, lf := range rows {
		byHost[lf.HostID] = append(byHost[lf.HostID], localForwardContent{
			BindScope:   lf.BindScope,
			LocalPort:   lf.LocalPort,
			TargetIP:    lf.TargetIP,
			TargetPort:  lf.TargetPort,
			Description: lf.Description,
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
// @Description  Each Host carries its local forwards in the file, and local_forwards in the answer counts them.
// @Tags         export and import
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.exportRequest  true  "The password that encrypts the file"
// @Success  200  {object}  models.Response{data=api.exportedTunnels}
// @Failure  400  {object}  api.errorBody  "The password is refused"
// @Router       /export/tunnels [post]
func (h *TransferHandler) ExportTunnels(c echo.Context) error {
	var req exportRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	refused := checkExportPassword(req.Password)
	if refused != nil {
		return refused.answer(c)
	}

	var hosts []models.Host

	err = h.hosts.db.Find(&hosts).Error
	if err != nil {
		h.hosts.logger.Error("failed to read the Hosts for an export", logid.TransferHostsReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errExportHostsReadFailed)
	}

	var sps []models.ServicePort

	err = h.hosts.db.Find(&sps).Error
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

			return failure(c, http.StatusInternalServerError, errExportHostSecretsSealed, errorArgs{"host": host.IP})
		}

		// A Host that carries nothing is written as an empty list and never as
		// nil, which is the difference the import reads: see hostContent.
		opened.AssignedLocalPorts = assigned[host.ID].localPorts
		if opened.AssignedLocalPorts == nil {
			opened.AssignedLocalPorts = []int{}
		}

		// The scopes are the other way about: a nil map is left nil so that the
		// field stays out of the file altogether, because every assignment
		// being on the wildcard is what no entry means.
		opened.AssignedBindScopes = assigned[host.ID].bindScopes

		// Never nil, for the reason AssignedLocalPorts is never nil.
		opened.LocalForwards = forwards[host.ID]
		if opened.LocalForwards == nil {
			opened.LocalForwards = []localForwardContent{}
		}

		exportedForwards += len(opened.LocalForwards)

		content.Hosts = append(content.Hosts, opened)
	}

	for _, sp := range sps {
		content.ServicePorts = append(content.ServicePorts, servicePortContent{
			ServiceIP:   sp.ServiceIP,
			ServicePort: sp.ServicePort,
			LocalPort:   sp.LocalPort,
			Description: sp.Description,
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

// ImportTunnels takes a file an export made and writes the Hosts and the
// service ports in it.
//
// Everything happens in one transaction, and anything that stops it rolls the
// whole of it back: a configuration that landed half way is one the operator
// has to take apart by hand before trying again, and the row that stopped the
// import is not always the last one.
//
// @Summary      Write what an exported tunnels file holds
// @Description  Adds what is not registered here and skips what is, naming in the answer what it skipped and why. Send the same file again with overwrite true to replace those rows instead.
// @Description  The whole import is one transaction: a file that is refused half way through leaves the database exactly as it was.
// @Description  A Host the import writes is left carrying the local forwards the file names for it, and keeps its own when the file has no local_forwards for it. The import is refused when the file opens one local port twice, or opens the port this server is stored to listen on or a local port a local forward of another Host holds here.
// @Tags         export and import
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.importRequest  true  "The password, the file and whether to overwrite"
// @Success  200  {object}  models.Response{data=api.importedTunnels}
// @Failure  400  {object}  api.errorBody  "The password is wrong, the file is damaged, it is not a file this program wrote, or it holds the other kind"
// @Router       /import/tunnels [post]
func (h *TransferHandler) ImportTunnels(c echo.Context) error {
	var req importRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
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

	refused = checkLocalForwards(c, content)
	if refused != nil {
		return refused.answer(c)
	}

	// Read before the transaction for the reason storedAPIPort gives, and only
	// for a file that carries a local forward, so that a file without one
	// reads nothing it has no use for.
	apiPort := 0

	if carriesLocalForwards(content) {
		apiPort, err = h.hosts.storedAPIPort()
		if err != nil {
			h.hosts.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
			return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
		}
	}

	tx := h.hosts.db.Begin()

	err = tx.Error
	if err != nil {
		h.hosts.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	items := make([]transferItem, 0, len(content.Hosts)+len(content.ServicePorts))

	// The Hosts whose assignments this import is to write: the ones it wrote.
	// A Host that was skipped is one that was registered here before this file
	// arrived, and what it carries was not asked to be replaced either.
	written := make([]hostContent, 0, len(content.Hosts))

	for _, host := range content.Hosts {
		item, refused := h.importHost(c, tx, host, req.Overwrite)
		if refused != nil {
			tx.Rollback()
			return refused.answer(c)
		}

		if item.Action != transferSkipped {
			written = append(written, host)
		}

		items = append(items, *item)
	}

	// The service ports are written after the Hosts, so that a file whose
	// service ports are refused takes the Hosts of that same file back out with
	// them. Which of the two comes first is otherwise of no consequence: the
	// tunnels are built by the loop from both together, and no row of one
	// points at a row of the other.
	for _, sp := range content.ServicePorts {
		item, refused := h.importServicePort(c, tx, sp, req.Overwrite)
		if refused != nil {
			tx.Rollback()
			return refused.answer(c)
		}

		items = append(items, *item)
	}

	// The assignments are written last, after both tables are in place. They
	// point at rows of both, and a service port of the file is created in the
	// loop above, so anything earlier would be looking for rows this same
	// import has not written yet.
	for _, host := range written {
		more, refused := h.importAssignments(tx, host, content)
		if refused != nil {
			tx.Rollback()
			return refused.answer(c)
		}

		items = append(items, more...)
	}

	more, refused := h.importLocalForwards(tx, written, apiPort)
	if refused != nil {
		tx.Rollback()
		return refused.answer(c)
	}

	items = append(items, more...)

	err = tx.Commit().Error
	if err != nil {
		h.hosts.logger.Error("failed to commit the transaction",
			logid.DatabaseTransactionCommitFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	answer := importedTunnels{Items: items}

	for _, item := range items {
		switch item.Action {
		case transferAdded:
			answer.Added++
		case transferReplaced:
			answer.Replaced++
		case transferSkipped:
			answer.Skipped++
		}
	}

	// The loop is woken after the commit, for the reason every write handler
	// wakes it there: a pass that runs before it reads the rows as they were.
	h.hosts.manager.WakeReconcile()

	h.hosts.logger.Info("imported a tunnel configuration",
		logid.TransferImported.Field(),
		zap.Bool("overwrite", req.Overwrite),
		zap.Int("added", answer.Added),
		zap.Int("replaced", answer.Replaced),
		zap.Int("skipped", answer.Skipped))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    answer,
	})
}

// importHost writes one Host of the file and reports what it did with it.
//
// It is checked here, inside the transaction, rather than in a pass of its own
// beforehand. The refusal then names the row that stopped the import while the
// rows before it are taken back out by the rollback, which is what makes "run
// it again once that row is fixed" the whole of the work left.
func (h *TransferHandler) importHost(c echo.Context, tx *gorm.DB, host hostContent,
	overwrite bool) (*transferItem, *refusal) {
	name := host.IP

	// The rules of a create are run on what the file carries, so that a file
	// that was written by hand cannot put into the database what the screens
	// refuse: a port out of range, or something that is not an address.
	err := c.Validate(&models.CreateHostRequest{
		IP:            host.IP,
		Port:          host.Port,
		User:          host.User,
		Password:      host.Password,
		PrivateKey:    host.PrivateKey,
		KeyPassphrase: host.KeyPassphrase,
		Description:   host.Description,
	})
	if err != nil {
		return nil, refuse(http.StatusBadRequest, errImportHostRefused, errorArgs{"host": name, "reason": err.Error()})
	}

	// The scopes the file names are held to the two words as well. The column
	// is under the same rule in the database, and a third word would otherwise
	// be met as a failed write halfway through the import, which names the row
	// and not what is wrong with it.
	unknown := unknownBindScope(host.AssignedBindScopes)
	if unknown != "" {
		return nil, refuse(http.StatusBadRequest, errImportHostRefused, errorArgs{"host": name, "reason": unknown})
	}

	if host.Password == "" && strings.TrimSpace(host.PrivateKey) == "" {
		return nil, refuse(http.StatusBadRequest, errImportHostNoLogin, errorArgs{"host": name})
	}

	var stored models.Host

	err = tx.Where("ip = ?", host.IP).First(&stored).Error
	found := err == nil

	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		h.hosts.logger.Error("failed to look for a Host while importing",
			logid.TransferHostLookupFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportHostsReadFailed)
	}

	if found && !overwrite {
		return &transferItem{
			Kind:   "host",
			Name:   name,
			Action: transferSkipped,
			Reason: "a Host with this IP is registered here already",
		}, nil
	}

	// Sealed with the key of this installation, which is what makes the file
	// work across installations: the secrets came in as plaintext and are
	// stored here the way a create on this machine stores them.
	password, err := h.hosts.sealPassword(host.Password)
	if err != nil {
		h.hosts.logger.Error("failed to encrypt the password of an imported Host",
			logid.TransferHostPasswordEncryptFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportHostPasswordEncrypt, errorArgs{"host": name})
	}

	privateKey, keyPassphrase, err := h.hosts.sealPrivateKey(host.PrivateKey, host.KeyPassphrase)
	if err != nil {
		var refusedKey *tunnel.KeyError
		if errors.As(err, &refusedKey) {
			return nil, refuse(http.StatusBadRequest, errImportHostKeyRefused, errorArgs{"host": name, "reason": refusedKey.Error()})
		}

		h.hosts.logger.Error("failed to encrypt the private key of an imported Host",
			logid.TransferHostPrivateKeyEncryptFailed.Field(),
			zap.Error(err))

		return nil, refuse(http.StatusInternalServerError, errImportHostKeyEncrypt, errorArgs{"host": name})
	}

	if found {
		// The row keeps its id, so the tunnels of that Host go on pointing at
		// it and the loop reconnects them rather than building them anew.
		stored.Port = host.Port
		stored.User = host.User
		stored.Password = password
		stored.PrivateKey = privateKey
		stored.KeyPassphrase = keyPassphrase
		stored.HostKey = host.HostKey
		// The pending key is dropped rather than kept. It was the key some
		// server presented to this installation while the row was trusted on
		// what it was trusted on before, and the answer to it is "is this the
		// right server". The file has just said what the right key is, so that
		// question is no longer the one being asked: approving the old pending
		// key would overwrite what was imported with a key the file did not
		// name. Dropping it costs nothing, since a server that still presents
		// something other than the imported key writes the pending key again
		// on the next connection, with what it presents now.
		stored.PendingHostKey = ""
		stored.Description = host.Description
		stored.Enabled = host.Enabled

		err = tx.Save(&stored).Error
		if err != nil {
			h.hosts.logger.Error("failed to replace a Host while importing",
				logid.TransferHostReplaceFailed.Field(),
				zap.Error(err))
			return nil, refuse(http.StatusInternalServerError, errImportHostReplaceFailed, errorArgs{"host": name})
		}

		return &transferItem{Kind: "host", Name: name, Action: transferReplaced}, nil
	}

	created := models.Host{
		IP:            host.IP,
		Port:          host.Port,
		User:          host.User,
		Password:      password,
		PrivateKey:    privateKey,
		KeyPassphrase: keyPassphrase,
		HostKey:       host.HostKey,
		Description:   host.Description,
		Enabled:       host.Enabled,
	}

	// The number is chosen the way the Hosts screen chooses it. An import that
	// left it to the column would step over a number the screen would have
	// handed out, so which of the two registered a Host would decide whether
	// the numbers have a gap in them.
	created.ID, err = nextHostID(tx)
	if err != nil {
		h.hosts.logger.Error("failed to work out the number for a Host being imported",
			logid.HostNextNumberReadFailed.Field(), zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportHostCreateFailed, errorArgs{"host": name})
	}

	err = tx.Create(&created).Error
	if err != nil {
		h.hosts.logger.Error("failed to create a Host while importing",
			logid.TransferHostCreateFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportHostCreateFailed, errorArgs{"host": name})
	}

	return &transferItem{Kind: "host", Name: name, Action: transferAdded}, nil
}

// importServicePort writes one service port of the file and reports what it did
// with it.
//
// A service port is held by two rules, not one: the service address with its
// port, and the local port. A row of the file can meet either of them, so both
// are looked up.
func (h *TransferHandler) importServicePort(c echo.Context, tx *gorm.DB, sp servicePortContent,
	overwrite bool) (*transferItem, *refusal) {
	name := sp.ServiceIP + ":" + strconv.Itoa(sp.ServicePort) + " on " + strconv.Itoa(sp.LocalPort)

	err := c.Validate(&models.CreateServicePortRequest{
		ServiceIP:   sp.ServiceIP,
		ServicePort: sp.ServicePort,
		LocalPort:   sp.LocalPort,
		Description: sp.Description,
	})
	if err != nil {
		return nil, refuse(http.StatusBadRequest, errImportServicePortRefused, errorArgs{"service_port": name, "reason": err.Error()})
	}

	var onService models.ServicePort

	err = tx.Where("service_ip = ? AND service_port = ?", sp.ServiceIP, sp.ServicePort).
		First(&onService).Error
	foundOnService := err == nil

	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		h.hosts.logger.Error("failed to look for a service port while importing",
			logid.TransferServicePortLookupFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportServicePortsReadFailed)
	}

	var onLocal models.ServicePort

	err = tx.Where("local_port = ?", sp.LocalPort).First(&onLocal).Error
	foundOnLocal := err == nil

	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		h.hosts.logger.Error("failed to look for a service port while importing",
			logid.TransferServicePortLookupFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportServicePortsReadFailed)
	}

	if !foundOnService && !foundOnLocal {
		created := models.ServicePort{
			ServiceIP:   sp.ServiceIP,
			ServicePort: sp.ServicePort,
			LocalPort:   sp.LocalPort,
			Description: sp.Description,
		}

		err = tx.Create(&created).Error
		if err != nil {
			h.hosts.logger.Error("failed to create a service port while importing",
				logid.TransferServicePortCreateFailed.Field(),
				zap.Error(err))
			return nil, refuse(http.StatusInternalServerError, errImportServicePortCreate, errorArgs{"service_port": name})
		}

		return &transferItem{Kind: "service_port", Name: name, Action: transferAdded}, nil
	}

	if !overwrite {
		reason := "the local port " + strconv.Itoa(sp.LocalPort) + " is in use here by another " +
			"service port"
		if foundOnService {
			reason = "a service port for " + sp.ServiceIP + ":" + strconv.Itoa(sp.ServicePort) +
				" is registered here already"
		}

		return &transferItem{
			Kind:   "service_port",
			Name:   name,
			Action: transferSkipped,
			Reason: reason,
		}, nil
	}

	// Two rows can stand in the way of one row of the file: one holding the
	// service address and another holding the local port. Replacing either of
	// them would leave the other breaking the rule it is under, and deleting
	// one of them is not what an overwrite was asked for, so this is refused
	// with the two rows named and the whole import is taken back.
	if foundOnService && foundOnLocal && onService.ID != onLocal.ID {
		return nil, refuse(http.StatusConflict, errImportServicePortTwoRows, errorArgs{"service_port": name,
			"service_address": sp.ServiceIP + ":" + strconv.Itoa(sp.ServicePort),
			"local_port":      strconv.Itoa(sp.LocalPort)})
	}

	stored := onService
	if !foundOnService {
		stored = onLocal
	}

	stored.ServiceIP = sp.ServiceIP
	stored.ServicePort = sp.ServicePort
	stored.LocalPort = sp.LocalPort
	stored.Description = sp.Description

	err = tx.Save(&stored).Error
	if err != nil {
		h.hosts.logger.Error("failed to replace a service port while importing",
			logid.TransferServicePortReplaceFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportServicePortReplace, errorArgs{"service_port": name})
	}

	return &transferItem{Kind: "service_port", Name: name, Action: transferReplaced}, nil
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

// importAssignments makes one Host of the file carry the service ports the file
// says it carries, and reports the ones it could not.
//
// What is stored for that Host is replaced rather than added to: the file says
// which service ports the Host carries, not which ones to add, so a Host the
// import wrote is left carrying what the file names and nothing besides. Only
// the Hosts this import wrote are touched, so the assignments of a Host that
// was skipped stay as they are.
//
// A local port the file names and this installation does not hold is left out
// with a word about it, and the rest of the import stands. It is how a file
// arrives whose service port was skipped for being registered here under
// another local port, and refusing the whole file for it would take across
// neither the Hosts nor the service ports that were fine, over an assignment
// the operator fixes by adding the service port and importing again. What is
// not done is passing over it in silence: every one of them is in the answer as
// a skipped item, with the Host and the local port named.
func (h *TransferHandler) importAssignments(tx *gorm.DB, host hostContent,
	content tunnelsContent) ([]transferItem, *refusal) {
	var stored models.Host

	err := tx.Where("ip = ?", host.IP).First(&stored).Error
	if err != nil {
		h.hosts.logger.Error("failed to read back a Host while importing its assignments",
			logid.TransferHostReadBackFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportAssignmentsHostRead, errorArgs{"host": host.IP})
	}

	wanted := host.AssignedLocalPorts
	if wanted == nil {
		// The file does not name them, which is a file written before they were
		// stored at all. Every Host carried every service port then, so that is
		// what it is taken to say: the service ports of that same file, which
		// are all the ones the installation it came from had.
		wanted = make([]int, 0, len(content.ServicePorts))
		for _, sp := range content.ServicePorts {
			wanted = append(wanted, sp.LocalPort)
		}
	}

	err = tx.Where("host_id = ?", stored.ID).Delete(&models.HostServicePort{}).Error
	if err != nil {
		h.hosts.logger.Error("failed to clear the assignments of a Host while importing",
			logid.TransferHostAssignmentsClearFailed.Field(),
			zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errImportAssignmentsClearFailed, errorArgs{"host": host.IP})
	}

	items := make([]transferItem, 0)
	// A hand-written file can name the same local port twice, and the pair of
	// columns is the primary key, so the second write of it would fail.
	assigned := make(map[uint]bool, len(wanted))

	for _, localPort := range wanted {
		name := host.IP + " carries " + strconv.Itoa(localPort)

		var sp models.ServicePort

		err = tx.Where("local_port = ?", localPort).First(&sp).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			items = append(items, transferItem{
				Kind:   "assignment",
				Name:   name,
				Action: transferSkipped,
				Reason: "no service port on the local port " + strconv.Itoa(localPort) +
					" is registered here, so there is nothing for the Host to carry",
			})

			continue
		}

		if err != nil {
			h.hosts.logger.Error("failed to look for a service port while importing an assignment",
				logid.TransferAssignmentServicePortLookupFailed.Field(),
				zap.Error(err))

			return nil, refuse(http.StatusInternalServerError, errImportServicePortsReadFailed)
		}

		if assigned[sp.ID] {
			continue
		}

		assigned[sp.ID] = true

		// Each assignment is written on the scope the file gives it, which a
		// file from before the scopes were stored answers with the bind
		// address it carries for the whole Host.
		err = tx.Create(&models.HostServicePort{HostID: stored.ID, SPID: sp.ID,
			BindScope: bindScopeOf(host, localPort)}).Error
		if err != nil {
			h.hosts.logger.Error("failed to store an assignment while importing",
				logid.TransferAssignmentStoreFailed.Field(),
				zap.Error(err))
			return nil, refuse(http.StatusInternalServerError, errImportAssignmentsStoreFailed, errorArgs{"host": host.IP})
		}
	}

	return items, nil
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
				BindScope:   lf.BindScope,
				LocalPort:   lf.LocalPort,
				TargetIP:    lf.TargetIP,
				TargetPort:  lf.TargetPort,
				Description: lf.Description,
			})
			if err != nil {
				return refuse(http.StatusBadRequest, errImportLocalForwardRefused,
					errorArgs{"host": host.IP, "local_port": localPort, "reason": err.Error()})
			}

			if openedBy[lf.LocalPort] {
				return refuse(http.StatusBadRequest, errImportLocalForwardDuplicate, errorArgs{"local_port": localPort})
			}

			openedBy[lf.LocalPort] = true
		}
	}

	return nil
}

// importLocalForwards makes each Host this import wrote carry the local
// forwards the file names for it and nothing besides, as importAssignments does
// for the service ports. A Host that was skipped keeps what it carries, and so
// does a Host the file names no local forwards for at all (hostContent).
//
// Every one of those Hosts is cleared before any row is written, so that a
// local port that moves from one Host of the file to another is free by the
// time it is written, whichever of the two comes first. What still holds the
// port after that belongs to a Host this import does not write, and taking it
// from that Host is not what an overwrite was asked for, so the import is
// refused with that Host named.
func (h *TransferHandler) importLocalForwards(tx *gorm.DB, written []hostContent,
	apiPort int) ([]transferItem, *refusal) {
	hostIDs := make([]uint, len(written))
	heldBefore := make([]map[int]bool, len(written))

	for i, host := range written {
		if host.LocalForwards == nil {
			continue
		}

		var stored models.Host

		err := tx.Where("ip = ?", host.IP).First(&stored).Error
		if err != nil {
			h.hosts.logger.Error("failed to read back a Host while importing its local forwards",
				logid.TransferHostReadBackFailed.Field(),
				zap.Error(err))
			return nil, refuse(http.StatusInternalServerError, errImportAssignmentsHostRead, errorArgs{"host": host.IP})
		}

		var held []models.LocalForward

		err = tx.Where("host_id = ?", stored.ID).Find(&held).Error
		if err != nil {
			h.hosts.logger.Error("failed to look for a local forward while importing",
				logid.TransferLocalForwardLookupFailed.Field(),
				zap.Error(err))
			return nil, refuse(http.StatusInternalServerError, errImportLocalForwardsReadFailed)
		}

		heldBefore[i] = make(map[int]bool, len(held))
		for _, lf := range held {
			heldBefore[i][lf.LocalPort] = true
		}

		err = tx.Where("host_id = ?", stored.ID).Delete(&models.LocalForward{}).Error
		if err != nil {
			h.hosts.logger.Error("failed to clear the local forwards of a Host while importing",
				logid.TransferHostLocalForwardsClearFailed.Field(),
				zap.Error(err))
			return nil, refuse(http.StatusInternalServerError, errImportLocalForwardsClearFailed, errorArgs{"host": host.IP})
		}

		hostIDs[i] = stored.ID
	}

	items := make([]transferItem, 0)

	for i, host := range written {
		for _, lf := range host.LocalForwards {
			localPort := strconv.Itoa(lf.LocalPort)
			name := host.IP + " opens " + localPort + " to " +
				net.JoinHostPort(lf.TargetIP, strconv.Itoa(lf.TargetPort))

			if lf.LocalPort == apiPort {
				return nil, refuse(http.StatusConflict, errImportLocalForwardAPIPort,
					errorArgs{"host": host.IP, "local_port": localPort})
			}

			var holder models.LocalForward

			err := tx.Where("local_port = ?", lf.LocalPort).First(&holder).Error
			if err == nil {
				// The Host is named by its address, which is how the screens
				// show it; a row whose Host is gone is named by the number.
				owner := strconv.FormatUint(uint64(holder.HostID), 10)

				var ownerHost models.Host
				if tx.First(&ownerHost, holder.HostID).Error == nil {
					owner = ownerHost.IP
				}

				return nil, refuse(http.StatusConflict, errImportLocalForwardPortTaken,
					errorArgs{"host": host.IP, "local_port": localPort, "owner": owner})
			}

			if !errors.Is(err, gorm.ErrRecordNotFound) {
				h.hosts.logger.Error("failed to look for a local forward while importing",
					logid.TransferLocalForwardLookupFailed.Field(),
					zap.Error(err))
				return nil, refuse(http.StatusInternalServerError, errImportLocalForwardsReadFailed)
			}

			// An empty scope is stored as the word it stands for, the way a
			// create stores it.
			bindScope := lf.BindScope
			if bindScope == "" {
				bindScope = models.BindScopeWildcard
			}

			err = tx.Create(&models.LocalForward{
				HostID:      hostIDs[i],
				BindScope:   bindScope,
				LocalPort:   lf.LocalPort,
				TargetIP:    lf.TargetIP,
				TargetPort:  lf.TargetPort,
				Description: lf.Description,
			}).Error
			if err != nil {
				h.hosts.logger.Error("failed to store a local forward while importing",
					logid.TransferLocalForwardStoreFailed.Field(),
					zap.Error(err))
				return nil, refuse(http.StatusInternalServerError, errImportLocalForwardsStoreFailed, errorArgs{"host": host.IP})
			}

			action := transferAdded
			if heldBefore[i][lf.LocalPort] {
				action = transferReplaced
			}

			items = append(items, transferItem{Kind: "local_forward", Name: name, Action: action})
		}
	}

	return items, nil
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
// @Param   body  body  api.exportRequest  true  "The password that encrypts the file"
// @Success  200  {object}  models.Response{data=api.exportedSettings}
// @Failure  400  {object}  api.errorBody  "The password is refused"
// @Router       /export/settings [post]
func (h *TransferHandler) ExportSettings(c echo.Context) error {
	var req exportRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	refused := checkExportPassword(req.Password)
	if refused != nil {
		return refused.answer(c)
	}

	stored, err := settings.Load(h.hosts.db)
	if err != nil {
		h.hosts.logger.Error("failed to read the settings for an export",
			logid.TransferSettingsReadForExportFailed.Field(),
			zap.Error(err))
		return failure(c, http.StatusInternalServerError, errSettingsReadFailed)
	}

	exportedAt := time.Now()

	sealed, err := h.seal(transferKindSettings, settingsOf(stored), req.Password, exportedAt)
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
// @Tags         export and import
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.importRequest  true  "The password and the file"
// @Success  200  {object}  models.Response{data=api.importedSettings}
// @Failure  400  {object}  api.errorBody  "The file does not open, or its settings do not pass the rules of the Settings screen"
// @Failure  409  {object}  api.errorBody{data=api.apiPortTaken}  "The api_port of the file is the local port of a local forward. Nothing was stored"
// @Router       /import/settings [post]
func (h *TransferHandler) ImportSettings(c echo.Context) error {
	var req importRequest

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	file, refused := h.open(req.File, req.Password, transferKindSettings)
	if refused != nil {
		return refused.answer(c)
	}

	stored, err := settings.Load(h.hosts.db)
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

	// The settings were read above, before the transaction, for the reason
	// storedAPIPort gives: the pool holds one connection.
	tx := h.hosts.db.Begin()

	err = tx.Error
	if err != nil {
		h.hosts.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// Held to the local forwards the way a save on the Settings screen is, and
	// only when the port changes, for the same reason.
	if updated.APIPort != before.APIPort {
		refused, err := apiPortRefused(tx, errImportSettingsAPIPortForward, updated.APIPort, before.APIPort)
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

	err = settings.Save(tx, &updated)
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
