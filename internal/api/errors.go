package api

import (
	"errors"
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
	errRequestFieldRenamed     errorCode = "request.field_renamed"
	errTransactionBeginFailed  errorCode = "db.transaction.begin_failed"
	errTransactionCommitFailed errorCode = "db.transaction.commit_failed"
	errListPageNotANumber      errorCode = "list.page.not_a_number"
	errListSizeUnsupported     errorCode = "list.size.unsupported"

	errAuthRequired                errorCode = "auth.session.required"
	errAuthCSRFRefused             errorCode = "auth.csrf.refused"
	errAuthCredentialsInvalid      errorCode = "auth.credentials.invalid"
	errAuthSessionCreateFailed     errorCode = "auth.session.create_failed"
	errAuthSetupRequired           errorCode = "auth.setup.required"
	errAuthSetupAlreadyDone        errorCode = "auth.setup.already_done"
	errAuthSetupFailed             errorCode = "auth.setup.failed"
	errAuthPasswordTooShort        errorCode = "auth.password.too_short"
	errAuthPasswordTooLong         errorCode = "auth.password.too_long"
	errAuthTooManyAttempts         errorCode = "auth.attempts.too_many"
	errAuthPasswordTooManyAttempts errorCode = "auth.password_attempts.too_many"
	errAuthTokenInvalid            errorCode = "auth.token.invalid"
	errAuthTokenExpired            errorCode = "auth.token.expired"
	errAuthTokenRouteRefused       errorCode = "auth.token.route_refused"
	errAuthTokenScopeMissing       errorCode = "auth.token.scope_missing"

	errTokenReadFailed        errorCode = "token.read_failed"
	errTokenRequestInvalid    errorCode = "token.request.invalid"
	errTokenNameEmpty         errorCode = "token.name.empty"
	errTokenNameTooLong       errorCode = "token.name.too_long"
	errTokenNameTaken         errorCode = "token.name.taken"
	errTokenScopesEmpty       errorCode = "token.scopes.empty"
	errTokenScopeUnknown      errorCode = "token.scope.unknown"
	errTokenExpiryUnsupported errorCode = "token.expiry.unsupported"
	errTokenCreateFailed      errorCode = "token.create_failed"
	errTokenIDInvalid         errorCode = "token.id.invalid"
	errTokenNotFound          errorCode = "token.not_found"
	errTokenDeleteFailed      errorCode = "token.delete_failed"

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
	errHostSocksPortRequired       errorCode = "host.socks_port.required"
	errHostSocksSourcesInvalid     errorCode = "host.socks_allowed_sources.invalid"
	errHostSocksPortIsAPIPort      errorCode = "host.socks_port.api_port"
	errHostSocksPortTaken          errorCode = "host.socks_port.taken"
	errHostSocksPortLocalForward   errorCode = "host.socks_port.local_forward"

	errServicePortIDInvalid        errorCode = "service_port.id.invalid"
	errServicePortNotFound         errorCode = "service_port.not_found"
	errServicePortFetchFailed      errorCode = "service_port.fetch_failed"
	errServicePortListFailed       errorCode = "service_port.list_failed"
	errServicePortCreateFailed     errorCode = "service_port.create_failed"
	errServicePortUpdateFailed     errorCode = "service_port.update_failed"
	errServicePortDeleteFailed     errorCode = "service_port.delete_failed"
	errServicePortCreateHostsRead  errorCode = "service_port.create.hosts_read_failed"
	errServicePortCreateHostsStore errorCode = "service_port.create.hosts_store_failed"

	errLocalForwardNumberInvalid errorCode = "local_forward.number.invalid"
	errLocalForwardNotFound      errorCode = "local_forward.not_found"
	errLocalForwardFetchFailed   errorCode = "local_forward.fetch_failed"
	errLocalForwardListFailed    errorCode = "local_forward.list_failed"
	errLocalForwardCreateFailed  errorCode = "local_forward.create_failed"
	errLocalForwardUpdateFailed  errorCode = "local_forward.update_failed"
	errLocalForwardDeleteFailed  errorCode = "local_forward.delete_failed"
	errLocalForwardPortTaken     errorCode = "local_forward.local_port.taken"
	errLocalForwardPortIsAPIPort errorCode = "local_forward.local_port.api_port"
	errLocalForwardPortSocks     errorCode = "local_forward.local_port.socks"
	errLocalForwardSourcesBad    errorCode = "local_forward.allowed_sources.invalid"

	errAssignmentAddAndRemove        errorCode = "assignment.add_and_remove"
	errAssignmentServicePortsMissing errorCode = "assignment.service_port.not_found"
	errAssignmentUpdateFailed        errorCode = "assignment.update_failed"

	errStatusDesiredCountFailed errorCode = "status.desired_count_failed"
	errStatusFetchFailed        errorCode = "status.fetch_failed"

	errSettingsReadFailed           errorCode = "settings.read_failed"
	errSettingsStoreFailed          errorCode = "settings.store_failed"
	errSettingsRefused              errorCode = "settings.refused"
	errSettingsLanguageUnsupported  errorCode = "settings.ui_language.unsupported"
	errSettingsAPIPortLocalForward  errorCode = "settings.api_port.local_forward"
	errSettingsAPIPortSocks         errorCode = "settings.api_port.socks"
	errSettingsAlertAfterInvalid    errorCode = "settings.alert_after.invalid"
	errSettingsWebhookURLInvalid    errorCode = "settings.alert_webhook_url.invalid"
	errSettingsSMTPHostInvalid      errorCode = "settings.smtp_host.invalid"
	errSettingsSMTPPortInvalid      errorCode = "settings.smtp_port.invalid"
	errSettingsSMTPSecurityInvalid  errorCode = "settings.smtp_security.invalid"
	errSettingsSMTPAuthInvalid      errorCode = "settings.smtp_auth.invalid"
	errSettingsSMTPFromRequired     errorCode = "settings.smtp_from.required"
	errSettingsSMTPFromInvalid      errorCode = "settings.smtp_from.invalid"
	errSettingsSMTPToRequired       errorCode = "settings.smtp_to.required"
	errSettingsSMTPToInvalid        errorCode = "settings.smtp_to.invalid"
	errSettingsSMTPUserRequired     errorCode = "settings.smtp_username.required"
	errSettingsSMTPPasswordSeal     errorCode = "settings.smtp_password.seal_failed"
	errSettingsSMTPPasswordRequired errorCode = "settings.smtp_password.required"
	errAlertTestWebhookOff          errorCode = "settings.alert_test.webhook_off"
	errAlertTestSMTPOff             errorCode = "settings.alert_test.smtp_off"
	errAlertTestFailed              errorCode = "settings.alert_test.failed"

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

	errLogsClearPasswordMissing errorCode = "logs.clear.password_missing"
	errLogsClearPasswordWrong   errorCode = "logs.clear.password_wrong"
	errLogsClearFailed          errorCode = "logs.clear.failed"

	errUpdateCheckFailed     errorCode = "update.check_failed"
	errUpdateNotInstallable  errorCode = "update.not_installable"
	errUpdatePasswordMissing errorCode = "update.password_missing"
	errUpdatePasswordWrong   errorCode = "update.password_wrong"
	errUpdateInstallFailed   errorCode = "update.install_failed"

	errExportPasswordRequired  errorCode = "export.password.required"
	errExportPasswordTooShort  errorCode = "export.password.too_short"
	errExportPasswordTooLong   errorCode = "export.password.too_long"
	errExportHostsReadFailed   errorCode = "export.hosts.read_failed"
	errExportServicePortsRead  errorCode = "export.service_ports.read_failed"
	errExportAssignmentsRead   errorCode = "export.assignments.read_failed"
	errExportLocalForwardsRead errorCode = "export.local_forwards.read_failed"
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
	errImportSettingsAPIPortForward errorCode = "import.settings.api_port.local_forward"
	errImportSettingsAPIPortSocks   errorCode = "import.settings.api_port.socks"

	errImportLocalForwardRefused      errorCode = "import.local_forward.refused"
	errImportLocalForwardDuplicate    errorCode = "import.local_forward.duplicate"
	errImportLocalForwardAPIPort      errorCode = "import.local_forward.local_port.api_port"
	errImportLocalForwardPortTaken    errorCode = "import.local_forward.local_port.taken"
	errImportLocalForwardsReadFailed  errorCode = "import.local_forwards.read_failed"
	errImportLocalForwardsClearFailed errorCode = "import.local_forwards.clear_failed"
	errImportLocalForwardsStoreFailed errorCode = "import.local_forwards.store_failed"
	errImportLocalForwardSocks        errorCode = "import.local_forward.local_port.socks"

	errImportSocksDuplicate    errorCode = "import.socks.duplicate"
	errImportSocksAPIPort      errorCode = "import.socks.socks_port.api_port"
	errImportSocksPortTaken    errorCode = "import.socks.socks_port.taken"
	errImportSocksLocalForward errorCode = "import.socks.socks_port.local_forward"

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
	errRequestFieldRenamed:     "The field {old} was renamed to {new}. Send {new} instead",
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
	// the hold has left to run, which is what the header beside it says. It
	// says nothing else: which of the two counters is holding, whether the
	// username sent exists and whether the password was right are all things
	// the sender is here to find out, and none of them is looked at before
	// this is raised. What the sentence leaves out it leaves out on purpose,
	// so there is nothing here to put back.
	errAuthTooManyAttempts: "Too many failed sign in attempts. Sign in is blocked for {retry_after} seconds",
	// The same hold, met by a call that asks the operator for the account
	// password again. It is one counter with the login, so the number is the
	// login's number; the sentence is its own because the sender of this is
	// already logged in, and a refusal that told them signing in was blocked
	// would name something they are not doing.
	errAuthPasswordTooManyAttempts: "Too many failed password attempts. Password checks are blocked for {retry_after} seconds",
	// The answers to a request that came with an API token in place of a
	// session. A token that is not there and one that was revoked are the same
	// answer, because a revoked token is a row that was deleted and nothing of
	// it is left to tell the two apart by.
	errAuthTokenInvalid: "The API token is not one this server made, or it was revoked",
	errAuthTokenExpired: "The API token ran out at {expires_at}. Make a new one on the Settings screen",
	// The route is one no scope opens. It says to sign in, because what these
	// routes change is the credentials themselves, and a token that could
	// reach them could make itself a session or another token.
	errAuthTokenRouteRefused: "{method} {path} cannot be reached with an API token. Sign in and use a session for it",
	errAuthTokenScopeMissing: "The API token was not made with the {scope} scope, which {method} {path} needs",

	// The API tokens themselves, as the Settings screen makes and revokes them.
	errTokenReadFailed:        "Failed to read the API tokens",
	errTokenRequestInvalid:    "Invalid request body. Send a JSON object with name, scopes and expires_in_days",
	errTokenNameEmpty:         "Name the token",
	errTokenNameTooLong:       "The name of a token must be at most {max} characters long",
	errTokenNameTaken:         "There is a token named {name} already",
	errTokenScopesEmpty:       "Give the token at least one scope",
	errTokenScopeUnknown:      "{scope} is not a scope. Use one of {scopes}",
	errTokenExpiryUnsupported: "expires_in_days must be one of {days}, and not {value}",
	errTokenCreateFailed:      "Failed to create the API token",
	errTokenIDInvalid:         "Invalid token ID: {reason}",
	errTokenNotFound:          "API token not found",
	errTokenDeleteFailed:      "Failed to revoke the API token",

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

	// The SOCKS5 proxy of a Host. The three refusals over its port are
	// conflicts for the reason the ones over the local port of a local forward
	// are, and each names what opens the port already.
	errHostSocksPortRequired:     "The SOCKS5 proxy is switched on without a port. Give socks_port a port from 1 to 65535",
	errHostSocksSourcesInvalid:   "The allowed sources of the SOCKS5 proxy are refused: {reason}",
	errHostSocksPortIsAPIPort:    "The SOCKS5 port {socks_port} is the port this server listens on",
	errHostSocksPortTaken:        "The SOCKS5 port {socks_port} is already opened by the SOCKS5 proxy of the Host {host}",
	errHostSocksPortLocalForward: "The SOCKS5 port {socks_port} is already opened by a local forward of the Host {host}",

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

	// The local forwards of a Host. The two refusals over the local port are
	// conflicts rather than a malformed body: the port is opened on this
	// machine, and a port something here already opens leaves one of the two
	// unable to start.
	errLocalForwardNumberInvalid: "Invalid local forward number: {reason}",
	errLocalForwardNotFound:      "Local forward not found",
	errLocalForwardFetchFailed:   "Failed to fetch local forward",
	errLocalForwardListFailed:    "Failed to fetch local forwards",
	errLocalForwardCreateFailed:  "Failed to create local forward",
	errLocalForwardUpdateFailed:  "Failed to update local forward",
	errLocalForwardDeleteFailed:  "Failed to delete local forward",
	errLocalForwardPortTaken:     "The local port {local_port} is already opened by another local forward",
	errLocalForwardPortIsAPIPort: "The local port {local_port} is the port this server listens on",
	errLocalForwardPortSocks:     "The local port {local_port} is already opened by the SOCKS5 proxy of the Host {host}",
	errLocalForwardSourcesBad:    "The allowed sources of the local forward are refused: {reason}",

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
	// The port a local forward opens, asked for as the port of this server. The
	// answer carries the forward and a free port in data, which is what the
	// screen offers to move one of the two to.
	errSettingsAPIPortLocalForward: "The settings are refused: the port {api_port} is opened by the local forward of the Host {host} to {target}. Move the local forward to another port, or choose another port for this server",
	// The same port opened by the SOCKS5 proxy of a Host. data carries the
	// proxy in socks_host beside the suggested port.
	errSettingsAPIPortSocks: "The settings are refused: the port {api_port} is opened by the SOCKS5 proxy of the Host {host}. Move the SOCKS5 proxy to another port, or choose another port for this server",
	// The alert settings. Each rule is raised under a code of its own for the
	// reason the language is: the value that was refused goes into a sentence
	// the screen writes in its own language, and the rule is in the sentence
	// rather than in an English {reason}.
	errSettingsAlertAfterInvalid:   "The settings are refused: {value} is not a delay an alert can wait for. Use a number of seconds from {min} to {max}",
	errSettingsWebhookURLInvalid:   "The settings are refused: {value} is not a webhook address. Use an address that begins with http:// or https://, or remove the stored address to turn the webhook off",
	errSettingsSMTPHostInvalid:     "The settings are refused: {value} is not the name or the address of a mail server",
	errSettingsSMTPPortInvalid:     "The settings are refused: {value} is not a port of a mail server. Use a number from 1 to 65535",
	errSettingsSMTPSecurityInvalid: "The settings are refused: {value} is not a connection security this server knows. Use none, starttls or tls",
	errSettingsSMTPAuthInvalid:     "The settings are refused: {value} is not a login method this server knows. Use none, plain or login",
	errSettingsSMTPFromRequired:    "The settings are refused: a mail server is set and no sender address is. Give the address alerts are sent from",
	errSettingsSMTPFromInvalid:     "The settings are refused: {value} is not a mail address",
	errSettingsSMTPToRequired:      "The settings are refused: a mail server is set and no recipient address is. Give at least one address to send alerts to",
	errSettingsSMTPToInvalid:       "The settings are refused: {value} is not a mail address. Separate several addresses with commas",
	errSettingsSMTPUserRequired:    "The settings are refused: the login method sends a user name and none is set. Give the user name, or set the login method to none",
	errSettingsSMTPPasswordSeal:    "Failed to encrypt the mail password",
	// The stored mail password is sent only to the server it was given for.
	// A body that moves the target has to give it again: see
	// guardStoredPassword.
	errSettingsSMTPPasswordRequired: "The settings are refused: the mail server, its port, the user name or the connection security changed, or the certificate check was turned off, and no new password was given. The stored password is sent only to the server it was given for. Type the password again, or set the login method to none",
	// The two test presses on the Settings screen. The failure carries what
	// went wrong in {reason}: it is what the webhook or the mail server said,
	// which is no sentence of this server's to translate.
	errAlertTestWebhookOff: "No webhook address is set, so there is nowhere to send a test alert to",
	errAlertTestSMTPOff:    "No mail server is set, so there is nothing to send a test alert through",
	errAlertTestFailed:     "The test alert was not delivered: {reason}",

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

	errLogsClearPasswordMissing: "Enter the password of your account to empty the log",
	errLogsClearPasswordWrong:   "That is not the password of this account. The log was not touched",
	errLogsClearFailed:          "The log file at {path} could not be emptied: {reason}",

	errUpdateCheckFailed:     "The newest release could not be read: {reason}",
	errUpdateNotInstallable:  "This installation cannot install an update from here. It is running as a program somebody started rather than as a registered service, so there is nothing to restart it afterwards",
	errUpdatePasswordMissing: "Enter the password of your account to install the update",
	errUpdatePasswordWrong:   "That is not the password of this account. Nothing was installed",
	errUpdateInstallFailed:   "The install could not be started: {reason}",

	// The export half of the transfer screens.
	errExportPasswordRequired:  "A password is required. It is what encrypts the file, and the file cannot be opened without it",
	errExportPasswordTooShort:  "The password must be at least {min} bytes long",
	errExportPasswordTooLong:   "The password must be at most {max} bytes long",
	errExportHostsReadFailed:   "Failed to read the Hosts",
	errExportServicePortsRead:  "Failed to read the service ports",
	errExportAssignmentsRead:   "Failed to read the service port assignments",
	errExportLocalForwardsRead: "Failed to read the local forwards",
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
	errImportSettingsAPIPortForward: "Nothing was imported. The file sets the port of this server to {api_port}, which the local forward of the Host {host} to {target} opens here. Move the local forward to another port and import again",
	errImportSettingsAPIPortSocks:   "Nothing was imported. The file sets the port of this server to {api_port}, which the SOCKS5 proxy of the Host {host} opens here. Move the SOCKS5 proxy to another port and import again",

	errImportLocalForwardRefused:      "Nothing was imported. The local forward on the local port {local_port} of the Host {host} in the file was refused: {reason}",
	errImportLocalForwardDuplicate:    "Nothing was imported. The file opens the local port {local_port} with more than one local forward",
	errImportLocalForwardAPIPort:      "Nothing was imported. The local forward of the Host {host} in the file opens the local port {local_port}, which is the port this server listens on",
	errImportLocalForwardPortTaken:    "Nothing was imported. The local forward of the Host {host} in the file opens the local port {local_port}, which a local forward of the Host {owner} opens here already. Change or delete one of the two and import again",
	errImportLocalForwardsReadFailed:  "Nothing was imported: failed to read the local forwards",
	errImportLocalForwardsClearFailed: "Nothing was imported: failed to replace the local forwards of the Host {host}",
	errImportLocalForwardsStoreFailed: "Nothing was imported: failed to store the local forwards of the Host {host}",
	errImportLocalForwardSocks:        "Nothing was imported. The local forward of the Host {host} in the file opens the local port {local_port}, which the SOCKS5 proxy of the Host {owner} opens here already. Change one of the two and import again",

	// The SOCKS5 proxies the file names. The fields of one are held to the
	// rules of a create under errImportHostRefused, since they are fields of
	// the Host.
	errImportSocksDuplicate:    "Nothing was imported. The file opens the port {port} more than once among the SOCKS5 proxies and the local forwards of its Hosts",
	errImportSocksAPIPort:      "Nothing was imported. The SOCKS5 proxy of the Host {host} in the file opens the port {socks_port}, which is the port this server listens on",
	errImportSocksPortTaken:    "Nothing was imported. The SOCKS5 proxy of the Host {host} in the file opens the port {socks_port}, which the SOCKS5 proxy of the Host {owner} opens here already. Change one of the two and import again",
	errImportSocksLocalForward: "Nothing was imported. The SOCKS5 proxy of the Host {host} in the file opens the port {socks_port}, which a local forward of the Host {owner} opens here already. Change one of the two and import again",

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

	return writeFailure(c, status, code, values, nil)
}

// writeFailure is failure with the data a refusal may carry beside its
// sentence. data is left out of the body when it is nil, so a refusal without
// any goes out in the shape it always did.
func writeFailure(c echo.Context, status int, code errorCode, values errorArgs, data interface{}) error {
	template, known := errorMessages[code]
	if !known {
		// A code with no sentence behind it is a bug in this package, and the
		// answer still has to say something. What goes out is the code itself,
		// marked as unnamed so that a screen does not go looking for it in a
		// phrase book, and the line in the log is what says where to look.
		c.Logger().Errorf("a refusal was raised under the error code %q, which no message is "+
			"registered for. The answer carries the code as its own text", code)

		return c.JSON(status, errorBody{
			Response: models.Response{Success: false, Data: data, Error: string(code)},
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
		Response: models.Response{Success: false, Data: data, Error: message},
		Code:     code,
		Args:     values,
	})
}

// refusal is an answer that says no, held as a value rather than written out on
// the spot. The step that found the problem is sometimes inside a transaction
// that has to be rolled back before anything reaches the client, and sometimes
// it is a helper with no echo.Context to write through.
type refusal struct {
	status   int
	code     errorCode
	args     errorArgs
	data     interface{}
	tooLarge bool
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

// carrying puts data in the answer beside the sentence. It is for a refusal a
// screen offers a way out of, where what the way out needs is more than the
// values written into the sentence.
func (r *refusal) carrying(data interface{}) *refusal {
	r.data = data

	return r
}

// answer writes the refusal out.
func (r *refusal) answer(c echo.Context) error {
	if r.tooLarge {
		return echo.ErrStatusRequestEntityTooLarge
	}

	return writeFailure(c, r.status, r.code, r.args, r.data)
}

// unreadableBody is the refusal of a request body that Bind or a read of it
// failed on.
//
// A body that ran past the body limit the server puts on every route (main.go,
// bodyLimit) is refused with 413 rather than 400. It is the answer the same
// body gets when its Content-Length already says it is too long, and it is
// handed to echo to write so that both go out the same way. The limit is only
// found out here when the body is chunked, and a client should not be told its
// body is malformed when what was wrong with it is its size.
func unreadableBody(err error) *refusal {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return &refusal{tooLarge: true}
	}

	return refuse(http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
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
