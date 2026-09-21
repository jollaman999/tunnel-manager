package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
)

// This file is the one place a refusal is written from. Every answer that says
// no carries an identifier next to the English sentence, so that a screen can
// show the sentence in the language it is running in instead of the one this
// server happens to speak.
//
// The English sentence stays in the answer. A script that reads this API has
// nothing but that sentence to go on, a screen that does not know a code yet
// still has something to show, and a client written before any of this existed
// reads the same body it always read.

// errorCode names a refusal. It is what a screen translates from, so the same
// refusal has to arrive under the same code wherever it is raised: a code per
// call site would be a phrase book with one entry per line of Go.
//
// The name is lowercase ASCII in dot separated segments. The first segment is
// what the refusal is about, which is the screen or the resource the operator
// was working on; the last segment is what went wrong; anything between them
// narrows the first. "host.not_found", "db.transaction.commit_failed",
// "import.host.key_refused". Words inside a segment are joined with an
// underscore, so that the dot stays the only thing that divides.
type errorCode string

// errorArgs are the values that were written into the English sentence, handed
// over on their own so that a screen can write them into a sentence of its own.
//
// They are keyed by name and not by position, because the order the values come
// in is a fact about the English sentence and not about the refusal: a language
// that puts the path before the reason cannot be served by a list.
type errorArgs map[string]string

// textCode names a string the server sends that is not a refusal: a sentence an
// answer carries, or the name of a thing the screen puts in a cell of its own.
// It is what a screen translates from, the way an errorCode is for an answer
// that says no and a logid.ID is for a line of the log.
//
// The English stays in the answer beside it. A script that reads this API has
// nothing but the English to go on, and a screen that has never heard of a code
// shows the English the answer carries, which is what a screen from before a
// code existed meets from a server that sends it.
//
// The name reads the way an errorCode does: lowercase ASCII in dot separated
// segments, the first of which is what the string belongs to. The codes
// themselves are declared beside the thing that sends them, because each of
// them goes out from one place and nowhere else; what holds them together is
// this kind, which is what the test over the catalogs reads them by.
type textCode string

// textArgs are the values written into one of those strings, handed over under
// their own names for the reason errorArgs are: the order English puts them in
// is a fact about English and not about the thing being said.
type textArgs map[string]string

// errorCodeUnspecified is what goes out when a refusal reaches this file with a
// code no message is registered for. It never appears while the table below and
// the call sites agree, and the test that walks the package catches a call site
// that stopped agreeing. It exists for the case that gets past both: a screen
// reading it knows the sentence is the English one and that nothing translated
// it, which is the thing a missing code must not be able to hide.
const errorCodeUnspecified errorCode = "unspecified"

// The codes. They are grouped the way the table below is, and the order of the
// two is the same so that an entry added to one is seen to be missing from the
// other.
const (
	errRequestBodyInvalid      errorCode = "request.body.invalid"
	errRequestValidationFailed errorCode = "request.validation.failed"
	errTransactionBeginFailed  errorCode = "db.transaction.begin_failed"
	errTransactionCommitFailed errorCode = "db.transaction.commit_failed"
	errListPageNotANumber      errorCode = "list.page.not_a_number"
	errListSizeUnsupported     errorCode = "list.size.unsupported"

	errAuthRequired            errorCode = "auth.session.required"
	errAuthCSRFRefused         errorCode = "auth.csrf.refused"
	errAuthCredentialsInvalid  errorCode = "auth.credentials.invalid"
	errAuthSessionCreateFailed errorCode = "auth.session.create_failed"
	errAuthSetupRequired       errorCode = "auth.setup.required"
	errAuthSetupAlreadyDone    errorCode = "auth.setup.already_done"
	errAuthSetupFailed         errorCode = "auth.setup.failed"
	errAuthPasswordTooShort    errorCode = "auth.password.too_short"
	errAuthPasswordTooLong     errorCode = "auth.password.too_long"
	errAuthTooManyAttempts     errorCode = "auth.attempts.too_many"

	errAccountReadFailed        errorCode = "account.read_failed"
	errAccountStoreFailed       errorCode = "account.store_failed"
	errAccountHashFailed        errorCode = "account.password.hash_failed"
	errAccountUsernameEmpty     errorCode = "account.username.empty"
	errAccountRequestInvalid    errorCode = "account.request.invalid"
	errAccountNothingToChange   errorCode = "account.nothing_to_change"
	errAccountPasswordWrong     errorCode = "account.current_password.wrong"
	errAccountUsernameUnchanged errorCode = "account.username.unchanged"
	errAccountPasswordUnchanged errorCode = "account.new_password.unchanged"
	errAccountNewPasswordShort  errorCode = "account.new_password.too_short"
	errAccountNewPasswordLong   errorCode = "account.new_password.too_long"

	errHostIDInvalid               errorCode = "host.id.invalid"
	errHostNotFound                errorCode = "host.not_found"
	errHostFetchFailed             errorCode = "host.fetch_failed"
	errHostListFailed              errorCode = "host.list_failed"
	errHostCreateFailed            errorCode = "host.create_failed"
	errHostUpdateFailed            errorCode = "host.update_failed"
	errHostDeleteFailed            errorCode = "host.delete_failed"
	errHostPasswordEncryptFailed   errorCode = "host.password.encrypt_failed"
	errHostKeyEncryptFailed        errorCode = "host.private_key.encrypt_failed"
	errHostCreateNoLogin           errorCode = "host.create.no_login"
	errHostCreateKeyRefused        errorCode = "host.create.key_refused"
	errHostCreateServicePortsRead  errorCode = "host.create.service_ports_read_failed"
	errHostCreateServicePortsStore errorCode = "host.create.service_ports_store_failed"
	errHostUpdateKeyRefused        errorCode = "host.update.key_refused"
	errHostUpdatePassphraseAlone   errorCode = "host.update.passphrase_without_key"
	errHostKeyNothingToApprove     errorCode = "host.host_key.nothing_to_approve"
	errHostKeyFingerprintChanged   errorCode = "host.host_key.fingerprint_changed"
	errHostKeyPasswordWrong        errorCode = "host.host_key.password_wrong"

	errServicePortIDInvalid        errorCode = "service_port.id.invalid"
	errServicePortNotFound         errorCode = "service_port.not_found"
	errServicePortFetchFailed      errorCode = "service_port.fetch_failed"
	errServicePortListFailed       errorCode = "service_port.list_failed"
	errServicePortCreateFailed     errorCode = "service_port.create_failed"
	errServicePortUpdateFailed     errorCode = "service_port.update_failed"
	errServicePortDeleteFailed     errorCode = "service_port.delete_failed"
	errServicePortCreateHostsRead  errorCode = "service_port.create.hosts_read_failed"
	errServicePortCreateHostsStore errorCode = "service_port.create.hosts_store_failed"

	errAssignmentAddAndRemove        errorCode = "assignment.add_and_remove"
	errAssignmentServicePortsMissing errorCode = "assignment.service_port.not_found"
	errAssignmentUpdateFailed        errorCode = "assignment.update_failed"

	errStatusDesiredCountFailed errorCode = "status.desired_count_failed"
	errStatusFetchFailed        errorCode = "status.fetch_failed"

	errSettingsReadFailed          errorCode = "settings.read_failed"
	errSettingsStoreFailed         errorCode = "settings.store_failed"
	errSettingsRefused             errorCode = "settings.refused"
	errSettingsLanguageUnsupported errorCode = "settings.ui_language.unsupported"

	errCertificateHTTPSOff       errorCode = "certificate.https_off"
	errCertificateServedUnread   errorCode = "certificate.served.read_failed"
	errCertificateRenewFailed    errorCode = "certificate.renew_failed"
	errCertificateInstallRefused errorCode = "certificate.install.refused"
	errCertificateStoreFailed    errorCode = "certificate.store_failed"
	errCertificateReadBackFailed errorCode = "certificate.read_back_failed"

	errLogsLinesNotANumber   errorCode = "logs.lines.not_a_number"
	errLogsLinesBelowOne     errorCode = "logs.lines.below_one"
	errLogsFileNotConfigured errorCode = "logs.file.not_configured"
	errLogsFileMissing       errorCode = "logs.file.missing"
	errLogsFileReadFailed    errorCode = "logs.file.read_failed"

	errExportPasswordRequired  errorCode = "export.password.required"
	errExportPasswordTooShort  errorCode = "export.password.too_short"
	errExportPasswordTooLong   errorCode = "export.password.too_long"
	errExportHostsReadFailed   errorCode = "export.hosts.read_failed"
	errExportServicePortsRead  errorCode = "export.service_ports.read_failed"
	errExportAssignmentsRead   errorCode = "export.assignments.read_failed"
	errExportHostSecretsSealed errorCode = "export.host.secrets_unreadable"
	errExportSealFailed        errorCode = "export.seal_failed"

	errImportFileMissing       errorCode = "import.file.missing"
	errImportPasswordRequired  errorCode = "import.password.required"
	errImportFileNotAnExport   errorCode = "import.file.not_an_export"
	errImportFileDamaged       errorCode = "import.file.damaged"
	errImportPasswordWrong     errorCode = "import.password.wrong"
	errImportFileOpenFailed    errorCode = "import.file.open_failed"
	errImportFileNotOurs       errorCode = "import.file.not_ours"
	errImportFileWrongKind     errorCode = "import.file.wrong_kind"
	errImportFileNewerFormat   errorCode = "import.file.newer_format"
	errImportFileNewerFormatBy errorCode = "import.file.newer_format_by"
	errImportFileEmpty         errorCode = "import.file.empty"

	errImportTunnelsUnreadable      errorCode = "import.tunnels.unreadable"
	errImportHostRefused            errorCode = "import.host.refused"
	errImportHostNoLogin            errorCode = "import.host.no_login"
	errImportHostsReadFailed        errorCode = "import.hosts.read_failed"
	errImportHostPasswordEncrypt    errorCode = "import.host.password_encrypt_failed"
	errImportHostKeyRefused         errorCode = "import.host.key_refused"
	errImportHostKeyEncrypt         errorCode = "import.host.private_key_encrypt_failed"
	errImportHostReplaceFailed      errorCode = "import.host.replace_failed"
	errImportHostCreateFailed       errorCode = "import.host.create_failed"
	errImportServicePortRefused     errorCode = "import.service_port.refused"
	errImportServicePortsReadFailed errorCode = "import.service_ports.read_failed"
	errImportServicePortCreate      errorCode = "import.service_port.create_failed"
	errImportServicePortTwoRows     errorCode = "import.service_port.two_rows"
	errImportServicePortReplace     errorCode = "import.service_port.replace_failed"
	errImportAssignmentsHostRead    errorCode = "import.assignments.host_read_failed"
	errImportAssignmentsClearFailed errorCode = "import.assignments.clear_failed"
	errImportAssignmentsStoreFailed errorCode = "import.assignments.store_failed"
	errImportSettingsUnreadable     errorCode = "import.settings.unreadable"
	errImportSettingsRefused        errorCode = "import.settings.refused"

	errUninstallPasswordWrong errorCode = "uninstall.password.wrong"
)

// errorBody is the body of every answer that says no.
//
// models.Response is embedded rather than copied, so that success and error keep
// the names and the shape they have always gone out under: encoding/json writes
// the fields of an embedded struct as if they were declared here. The two new
// fields come after them.
//
// error_code is written even when it is empty, because a screen has to be able
// to tell a server that names its refusals from one that does not. error_args is
// left out when the sentence has nothing written into it.
type errorBody struct {
	models.Response
	Code errorCode `json:"error_code"`
	Args errorArgs `json:"error_args,omitempty"`
}

// errorMessages is the English sentence each code stands for.
//
// A sentence with a value in it carries the value as {name}, and the name is the
// key the value arrives under in error_args. This is the only place these
// sentences are written: a refusal raised in three handlers is one entry here,
// which is also what makes it one entry in the phrase book of a screen.
var errorMessages = map[errorCode]string{
	// The shape of the request, and the database work every handler does.
	errRequestBodyInvalid:      "Invalid request body: {reason}",
	errRequestValidationFailed: "Validation failed: {reason}",
	errTransactionBeginFailed:  "Failed to start transaction",
	errTransactionCommitFailed: "Failed to commit transaction",
	errListPageNotANumber:      "The list was not read: page is not a number: {page}",
	errListSizeUnsupported:     "The list was not read: size must be one of {sizes}, and not {size}",

	// The session, the CSRF token and the setup gate.
	errAuthRequired: "Authentication required",
	// The answer to a state changing request that did not bring its token
	// back. It says what to send, because the client can fix it. It must not
	// read like the setup refusal: the UI moves to the setup screen on a 403
	// that mentions one. The header and the cookie are values rather than words
	// of the sentence, so that renaming either does not go past a translator.
	errAuthCSRFRefused:         "The request carries no valid {header} header. Send the token of the session, which the login answers with and the {cookie} cookie holds, on every POST, PUT and DELETE",
	errAuthCredentialsInvalid:  invalidCredentialsMessage,
	errAuthSessionCreateFailed: "Failed to create a session",
	// The answer to a request that a logged in client is not allowed to make
	// yet. It names what is missing, since the client can fix it and the UI
	// sends the operator to the setup screen on the strength of it.
	errAuthSetupRequired:    "The account setup is not finished. Set a username and a password through {path} first",
	errAuthSetupAlreadyDone: setupAlreadyDoneMessage,
	errAuthSetupFailed:      "Failed to set up the account",
	errAuthPasswordTooShort: "Password must be at least {min} bytes long",
	errAuthPasswordTooLong:  "Password must be at most {max} bytes long, because that is as far as bcrypt reads",
	// The answer to a login that is not being checked at all. It says how long
	// the hold has left to run, which is what the header beside it says, and it
	// says nothing else: which of the two counters is holding, whether the
	// username sent exists and whether the password was right are all things
	// the sender is here to find out, and none of them is looked at before this
	// is raised.
	errAuthTooManyAttempts: "Too many failed sign in attempts. No password is checked for the next {retry_after} seconds. Both the address an attempt came from and the account are counted, and this answer does not say which of the two was reached",

	// The one account this API is served behind.
	errAccountReadFailed:        "Failed to read the account",
	errAccountStoreFailed:       "Failed to store the account",
	errAccountHashFailed:        "Failed to hash the password",
	errAccountUsernameEmpty:     "Username must not be empty",
	errAccountRequestInvalid:    "Invalid request body. Send a JSON object with current_password and username, new_password or both",
	errAccountNothingToChange:   accountNothingToChangeMessage,
	errAccountPasswordWrong:     accountWrongPasswordMessage,
	errAccountUsernameUnchanged: "The username is the one the account already has",
	errAccountPasswordUnchanged: "The new password is the one the account already has",
	errAccountNewPasswordShort:  "The new password must be at least {min} bytes long",
	errAccountNewPasswordLong:   "The new password must be at most {max} bytes long, because that is as far as bcrypt reads",

	// The Hosts.
	errHostIDInvalid:               "Invalid Host ID: {reason}",
	errHostNotFound:                "Host not found",
	errHostFetchFailed:             "Failed to fetch Host",
	errHostListFailed:              "Failed to fetch Hosts",
	errHostCreateFailed:            "Failed to create Host",
	errHostUpdateFailed:            "Failed to update Host",
	errHostDeleteFailed:            "Failed to delete Host",
	errHostPasswordEncryptFailed:   "Failed to encrypt the password",
	errHostKeyEncryptFailed:        "Failed to encrypt the private key",
	errHostCreateNoLogin:           "The Host was not created: it carries no way to log in. Give a private key, a password, or both",
	errHostCreateKeyRefused:        "The Host was not created: {reason}",
	errHostCreateServicePortsRead:  "The Host was not created: failed to read the service ports",
	errHostCreateServicePortsStore: "The Host was not created: failed to store the service ports it carries",
	errHostUpdateKeyRefused:        "The Host was not updated: {reason}",
	errHostUpdatePassphraseAlone:   "The Host was not updated: a passphrase was sent without a private key. The two are checked together, so send the key along with it",
	errHostKeyNothingToApprove:     "The host key was not approved: this Host has no key waiting to be approved. Either it has been approved already, or nothing has connected to this Host since the last one was",
	// The refusal that says the question changed under the answer. The
	// fingerprint that is waiting goes into it, because the screen the
	// operator answered from is showing another one and the sentence has to be
	// able to say so without a second request.
	errHostKeyFingerprintChanged: "The host key was not approved: the fingerprint sent is not the one waiting to be approved, which is {waiting}. The SSH server presented another key after the one on the screen was read. Compare the fingerprint above against the server itself before approving it",
	errHostKeyPasswordWrong:      "The host key was not approved: the password does not open this account",

	// The service ports.
	errServicePortIDInvalid:        "Invalid service port ID: {reason}",
	errServicePortNotFound:         "Service port not found",
	errServicePortFetchFailed:      "Failed to fetch service port",
	errServicePortListFailed:       "Failed to fetch service ports",
	errServicePortCreateFailed:     "Failed to create service port",
	errServicePortUpdateFailed:     "Failed to update service port",
	errServicePortDeleteFailed:     "Failed to delete service port",
	errServicePortCreateHostsRead:  "The service port was not created: failed to read the Hosts",
	errServicePortCreateHostsStore: "The service port was not created: failed to store the Hosts that carry it",

	// Which service ports a Host carries.
	errAssignmentAddAndRemove:        "The change names the same service port to add and to remove: {ids}",
	errAssignmentServicePortsMissing: "No such service port: {ids}",
	errAssignmentUpdateFailed:        "Failed to update the service ports of the Host",

	// The tunnels as they are running.
	errStatusDesiredCountFailed: "Failed to count the tunnels that should be running",
	errStatusFetchFailed:        "Failed to fetch tunnel status",

	// The settings, read and written from the Settings screen and from an import.
	errSettingsReadFailed:  "Failed to read the settings",
	errSettingsStoreFailed: "Failed to store the settings",
	errSettingsRefused:     "The settings are refused: {reason}",
	// The one rule of the settings that is raised under a code of its own. A
	// screen showing it has to list the languages that would be taken, and a
	// list arriving inside {reason} as English prose is one it cannot use.
	errSettingsLanguageUnsupported: "The settings are refused: {language} is not a language this installation is drawn in. Use one of {languages}, or leave it empty to show each browser the language it asks for",

	// The TLS certificate this installation serves with.
	errCertificateHTTPSOff:       "No certificate is in use, because HTTPS is turned off. Turn on \"Serve over HTTPS\" and start tunnel-manager again",
	errCertificateServedUnread:   "Failed to read the certificate being served",
	errCertificateRenewFailed:    "Failed to make a new certificate. Nothing was changed",
	errCertificateInstallRefused: "The certificate was not stored: {reason}",
	errCertificateStoreFailed:    "Failed to store the certificate. Nothing was changed",
	errCertificateReadBackFailed: "Failed to read the new certificate back",

	// The log screen.
	errLogsLinesNotANumber:   "lines has to be a whole number",
	errLogsLinesBelowOne:     "lines has to be one or more",
	errLogsFileNotConfigured: "No log file is configured, so the logs are written to the console only. Set a log file on the Settings screen and start the server again.",
	errLogsFileMissing:       "There is no file at {path}. Either nothing has been logged to it yet, or the server could not open it at startup and is writing to the console only.",
	errLogsFileReadFailed:    "The log file at {path} cannot be read: {reason}",

	// The export half of the transfer screens.
	errExportPasswordRequired:  "A password is required. It is what encrypts the file, and the file cannot be opened without it",
	errExportPasswordTooShort:  "The password must be at least {min} bytes long",
	errExportPasswordTooLong:   "The password must be at most {max} bytes long",
	errExportHostsReadFailed:   "Failed to read the Hosts",
	errExportServicePortsRead:  "Failed to read the service ports",
	errExportAssignmentsRead:   "Failed to read the service port assignments",
	errExportHostSecretsSealed: "No export was made: the stored secrets of the Host {host} do not open with the encryption key of this installation",
	errExportSealFailed:        "Failed to encrypt the file",

	// Opening the file an import was sent.
	errImportFileMissing:      "No file was sent. Send the text an export answered with in the 'file' field",
	errImportPasswordRequired: "A password is required. It is the one the file was encrypted with at the installation it came from",
	errImportFileNotAnExport:  "This is not a file tunnel-manager exported. An exported file is one line of text that starts with a marker naming the format, and this one does not",
	errImportFileDamaged:      "The file is damaged. It carries the marker of an exported file, but the text after it was cut or altered, so no password opens it. Export it again",
	errImportPasswordWrong:    "The password does not open this file. It is the password that was typed at the export, not the password of this account",
	errImportFileOpenFailed:   "Failed to open the file",
	errImportFileNotOurs:      "The file opened with this password but does not hold what an export writes. It was encrypted with the password of this program by something else",
	errImportFileWrongKind:    "This file holds {found}, and this call takes {wanted}. Send it to the other import",
	errImportFileNewerFormat:  "The file is in format version {version} and this version of tunnel-manager reads up to {supported}. It was written by a newer version",
	// The same refusal from a file that says which version wrote it. It is a
	// code of its own rather than the one above with an empty value, because a
	// sentence with a hole where the version should be is not one a translator
	// can write.
	errImportFileNewerFormatBy: "The file is in format version {version} and this version of tunnel-manager reads up to {supported}. It was written by a newer version ({exported_by})",
	errImportFileEmpty:         "The file carries no content",

	// Writing what the file holds.
	errImportTunnelsUnreadable:      "The file says it holds the tunnel configuration, but the configuration in it cannot be read",
	errImportHostRefused:            "Nothing was imported. The Host {host} in the file was refused: {reason}",
	errImportHostNoLogin:            "Nothing was imported. The Host {host} in the file carries no way to log in: it has neither a private key nor a password",
	errImportHostsReadFailed:        "Nothing was imported: failed to read the Hosts",
	errImportHostPasswordEncrypt:    "Nothing was imported: failed to encrypt the password of the Host {host}",
	errImportHostKeyRefused:         "Nothing was imported. The private key of the Host {host} in the file was refused: {reason}",
	errImportHostKeyEncrypt:         "Nothing was imported: failed to encrypt the private key of the Host {host}",
	errImportHostReplaceFailed:      "Nothing was imported: failed to replace the Host {host}",
	errImportHostCreateFailed:       "Nothing was imported: failed to create the Host {host}",
	errImportServicePortRefused:     "Nothing was imported. The service port {service_port} in the file was refused: {reason}",
	errImportServicePortsReadFailed: "Nothing was imported: failed to read the service ports",
	errImportServicePortCreate:      "Nothing was imported: failed to create the service port {service_port}",
	errImportServicePortTwoRows:     "Nothing was imported. The service port {service_port} in the file meets two rows that are registered here: {service_address} belongs to one and the local port {local_port} to another. Delete one of the two and import again",
	errImportServicePortReplace:     "Nothing was imported: failed to replace the service port {service_port}",
	errImportAssignmentsHostRead:    "Nothing was imported: failed to read the Host {host}",
	errImportAssignmentsClearFailed: "Nothing was imported: failed to replace the service ports the Host {host} carries",
	errImportAssignmentsStoreFailed: "Nothing was imported: failed to store the service ports the Host {host} carries",
	errImportSettingsUnreadable:     "The file says it holds the settings of the manager, but the settings in it cannot be read",
	errImportSettingsRefused:        "Nothing was imported. The settings in the file are refused: {reason}",

	// Removing the installation.
	errUninstallPasswordWrong: "The password does not open this account",
}

// failure writes a refusal.
//
// args is variadic so that the refusals with nothing written into them, which
// are most of them, are raised without a nil. Only the first is read; a second
// one is a call that meant to pass one map.
func failure(c echo.Context, status int, code errorCode, args ...errorArgs) error {
	var values errorArgs
	if len(args) > 0 {
		values = args[0]
	}

	template, known := errorMessages[code]
	if !known {
		// A code with no sentence behind it is a bug in this package, and the
		// answer still has to say something. What goes out is the code itself,
		// marked as unnamed so that a screen does not go looking for it in a
		// phrase book, and the line in the log is what says where to look.
		c.Logger().Errorf("a refusal was raised under the error code %q, which no message is "+
			"registered for. The answer carries the code as its own text", code)

		return c.JSON(status, errorBody{
			Response: models.Response{Success: false, Error: string(code)},
			Code:     errorCodeUnspecified,
			Args:     values,
		})
	}

	message, filled := renderErrorMessage(template, values)
	if !filled {
		// The sentence went out with {name} still in it, which is a value the
		// call site did not hand over under the name the sentence asks for.
		c.Logger().Errorf("the message of the error code %q was written with a value missing, so "+
			"the answer carries a placeholder", code)
	}

	return c.JSON(status, errorBody{
		Response: models.Response{Success: false, Error: message},
		Code:     code,
		Args:     values,
	})
}

// refusal is an answer that says no, held as a value rather than written out on
// the spot. The step that found the problem is sometimes inside a transaction
// that has to be rolled back before anything reaches the client, and sometimes
// it is a helper with no echo.Context to write through.
type refusal struct {
	status int
	code   errorCode
	args   errorArgs
}

// refuse builds one. args is variadic for the reason failure's is.
func refuse(status int, code errorCode, args ...errorArgs) *refusal {
	values := errorArgs(nil)
	if len(args) > 0 {
		values = args[0]
	}

	return &refusal{status: status, code: code, args: values}
}

// named marks the values of the refusal that are themselves strings the server
// names. Such a value is handed over twice: as the English written into the
// sentence, under the name the sentence asks for, and as a textCode under that
// name with "_code" on the end. A screen that knows the code says the value in
// its own language, and one that does not shows the English beside it, the
// way it does for a code it has no sentence for. values is what the phrases
// behind those codes are written with, handed over under the names they use.
//
// They are added here rather than written at the call site so that what the
// call site hands over stays exactly what the sentence asks for, which is what
// the test over the call sites holds it to.
func (r *refusal) named(codes map[string]textCode, values textArgs) *refusal {
	if r.args == nil {
		r.args = errorArgs{}
	}

	for name, code := range codes {
		r.args[name+"_code"] = string(code)
	}

	for name, value := range values {
		r.args[name] = value
	}

	return r
}

// answer writes the refusal out.
func (r *refusal) answer(c echo.Context) error {
	return failure(c, r.status, r.code, r.args)
}

// renderErrorMessage writes the values into the sentence and reports whether
// every place in it was filled.
//
// It walks the sentence once rather than replacing name by name, so that a
// value which happens to hold braces of its own is written out as it is instead
// of being read as another place to fill.
func renderErrorMessage(template string, args errorArgs) (string, bool) {
	if !strings.ContainsRune(template, '{') {
		return template, true
	}

	var out strings.Builder

	filled := true
	rest := template

	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			break
		}

		closed := strings.IndexByte(rest[open:], '}')
		if closed < 0 {
			break
		}

		name := rest[open+1 : open+closed]

		value, ok := args[name]
		if !ok {
			// The place is left as it stands. A sentence with {reason} in it
			// says more about what happened than one with a hole in it.
			filled = false
			value = rest[open : open+closed+1]
		}

		out.WriteString(rest[:open])
		out.WriteString(value)

		rest = rest[open+closed+1:]
	}

	out.WriteString(rest)

	return out.String(), filled
}

// errorCodePlaceholders is the names a code writes values under, read out of the
// sentence it stands for. The test that holds the table against the call sites
// uses it, and so does anyone generating a phrase book from this package.
func errorCodePlaceholders(code errorCode) []string {
	template, known := errorMessages[code]
	if !known {
		return nil
	}

	names := make([]string, 0)
	seen := make(map[string]bool)
	rest := template

	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			break
		}

		closed := strings.IndexByte(rest[open:], '}')
		if closed < 0 {
			break
		}

		name := rest[open+1 : open+closed]
		if !seen[name] {
			seen[name] = true

			names = append(names, name)
		}

		rest = rest[open+closed+1:]
	}

	sort.Strings(names)

	return names
}

// unauthenticated answers a request that carries no usable session.
func unauthenticated(c echo.Context) error {
	return failure(c, http.StatusUnauthorized, errAuthRequired)
}
