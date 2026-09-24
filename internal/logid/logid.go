// Package logid holds the identifier every log line is written with.
//
// The log file stays English. It is what an operator greps and what a support
// request carries, and a file whose language followed the screen would be
// written in whatever language the last person selected, with the lines from
// before the change in another one. What the Logs screen shows, on the other
// hand, has to be in the language that was selected, and a sentence cannot be
// translated after the fact: the text in the file is the English one, and
// matching it against a catalogue by its words would break on the day somebody
// fixes a typo in it.
//
// So every line carries an identifier of its own, written as a field beside
// the message. The file keeps the English sentence, and the screen looks the
// identifier up to find the sentence to show. The values the sentence needs
// are already on the line as named fields, so a translation names them where
// its own grammar wants them rather than in the order English puts them in.
//
// The identifiers live here, in one package under internal, because every
// package that logs uses them and the screen needs one place to read the whole
// set out of. It imports nothing of this application, so no package that logs
// can be kept from importing it.
package logid

import "go.uber.org/zap"

// FieldKey is the field the identifier is written under. It is a single key
// for the whole application, so a reader of the log file finds every line that
// carries one by this name alone.
const FieldKey = "log_id"

// ID identifies one log line. Two places that say the same thing carry the
// same ID, so the screen translates it once and both lines are covered.
type ID string

// Field returns the field that carries the ID on a log line. It goes first,
// before the fields the message needs, so that the identifier of a line is
// always in the same place.
//
//	t.logger.Info("tunnel connected successfully",
//		logid.TunnelConnected.Field(),
//		zap.String("local", t.Local.String()))
func (id ID) Field() zap.Field {
	return zap.String(FieldKey, string(id))
}

// The identifiers, by area.
//
// An ID reads "<area>.<event>": the area is the job the line belongs to, and
// the event is what happened, in snake_case. The Go constant is the same thing
// in upper camel case, which TestConstantNamesFollowTheirIDs holds it to, so
// the two cannot drift apart.
//
// Neither part names a file or a line. Code moves between files, and an ID
// that named the file it used to live in would send whoever reads the
// catalogue to the wrong place, while the screen would keep showing the line
// under a name that is no longer true.
//
// An area is not a package name either. It is the job: the tunnels are
// "tunnel" wherever in the package they are logged from, and the descriptor
// limit is "ulimit" though it is logged from package main.

// tunnel: the SSH tunnels, their connections and the reconcile loop.
const (
	TunnelStatusSaveFailed            ID = "tunnel.status_save_failed"
	TunnelReconnectWaiting            ID = "tunnel.reconnect_waiting"
	TunnelServerUnreachable           ID = "tunnel.server_unreachable"
	TunnelKeepaliveFailed             ID = "tunnel.keepalive_failed"
	TunnelForwardUnreachable          ID = "tunnel.forward_unreachable"
	TunnelRemoteDialFailed            ID = "tunnel.remote_dial_failed"
	TunnelForwardCopyFailed           ID = "tunnel.forward_copy_failed"
	TunnelSshConnectFailed            ID = "tunnel.ssh_connect_failed"
	TunnelRemoteListenerFailed        ID = "tunnel.remote_listener_failed"
	TunnelConnected                   ID = "tunnel.connected"
	TunnelConnectionClosed            ID = "tunnel.connection_closed"
	TunnelListenerAcceptFailed        ID = "tunnel.listener_accept_failed"
	TunnelStarting                    ID = "tunnel.starting"
	TunnelConnectFailedGivingUp       ID = "tunnel.connect_failed_giving_up"
	TunnelConnectFailedRetrying       ID = "tunnel.connect_failed_retrying"
	TunnelHostPasswordUndecryptable   ID = "tunnel.host_password_undecryptable"
	TunnelHostPasswordEncryptFailed   ID = "tunnel.host_password_encrypt_failed"
	TunnelHostPasswordStoreFailed     ID = "tunnel.host_password_store_failed"
	TunnelHostPasswordEncrypted       ID = "tunnel.host_password_encrypted"
	TunnelHostKeyUnusablePasswordUsed ID = "tunnel.host_key_unusable_password_used"
	TunnelHostKeyUnreadable           ID = "tunnel.host_key_unreadable"
	TunnelHostSecretUndecryptable     ID = "tunnel.host_secret_undecryptable"
	TunnelStartSkippedHostDisabled    ID = "tunnel.start_skipped_host_disabled"
	TunnelHostTunnelsFetchFailed      ID = "tunnel.host_tunnels_fetch_failed"
	TunnelTunnelsFetchFailed          ID = "tunnel.tunnels_fetch_failed"
	TunnelStatusResetFailed           ID = "tunnel.status_reset_failed"
	TunnelRestoreFailed               ID = "tunnel.restore_failed"
	TunnelRestored                    ID = "tunnel.restored"
	TunnelKeyUnreadable               ID = "tunnel.key_unreadable"
	TunnelStopSkippedNotRunning       ID = "tunnel.stop_skipped_not_running"
	TunnelStopFailed                  ID = "tunnel.stop_failed"
	TunnelStartFailed                 ID = "tunnel.start_failed"
	TunnelRestartFailed               ID = "tunnel.restart_failed"
	TunnelReconcileFailed             ID = "tunnel.reconcile_failed"
	TunnelReconciled                  ID = "tunnel.reconciled"
	TunnelManagerCreateFailed         ID = "tunnel.manager_create_failed"
	TunnelRestoreStarting             ID = "tunnel.restore_starting"
	TunnelReconcileStopTimedOut       ID = "tunnel.reconcile_stop_timed_out"
	// The local forwards. What they share with the tunnels, the SSH connection
	// and the copy, is logged under the IDs above.
	TunnelLocalForwardConnected             ID = "tunnel.local_forward_connected"
	TunnelLocalForwardConnectFailedRetrying ID = "tunnel.local_forward_connect_failed_retrying"
	TunnelLocalForwardConnectFailedGivingUp ID = "tunnel.local_forward_connect_failed_giving_up"
	TunnelLocalForwardListenFailed          ID = "tunnel.local_forward_listen_failed"
	TunnelLocalForwardTargetDialFailed      ID = "tunnel.local_forward_target_dial_failed"
	// The SOCKS5 proxies of the Hosts. What they share with the local
	// forwards, the SSH connection, the listeners and the copy, is logged
	// under the IDs above.
	TunnelSocksConnected             ID = "tunnel.socks_connected"
	TunnelSocksConnectFailedRetrying ID = "tunnel.socks_connect_failed_retrying"
	TunnelSocksConnectFailedGivingUp ID = "tunnel.socks_connect_failed_giving_up"
	TunnelSocksListenFailed          ID = "tunnel.socks_listen_failed"
	TunnelSocksHandshakeFailed       ID = "tunnel.socks_handshake_failed"
	TunnelSocksTargetDialFailed      ID = "tunnel.socks_target_dial_failed"
	TunnelSocksSourceRefused         ID = "tunnel.socks_source_refused"
)

// database: opening the database and what the queries report.
const (
	DatabaseGormMessage             ID = "database.gorm_message"
	DatabaseQueryFailed             ID = "database.query_failed"
	DatabaseQuerySlow               ID = "database.query_slow"
	DatabaseQuery                   ID = "database.query"
	DatabaseServicePortsFilled      ID = "database.service_ports_filled"
	DatabaseOpened                  ID = "database.opened"
	DatabaseOpenFailed              ID = "database.open_failed"
	DatabaseTransactionStartFailed  ID = "database.transaction_start_failed"
	DatabaseTransactionCommitFailed ID = "database.transaction_commit_failed"
)

// tlsserve: the API port, which serves TLS and plain HTTP at once.
const (
	TlsserveAcceptFailed            ID = "tlsserve.accept_failed"
	TlsservePeekDeadlineSetFailed   ID = "tlsserve.peek_deadline_set_failed"
	TlsservePeekTimedOut            ID = "tlsserve.peek_timed_out"
	TlsservePeekDeadlineClearFailed ID = "tlsserve.peek_deadline_clear_failed"
	TlsserveRedirectedToHttps       ID = "tlsserve.redirected_to_https"
)

// ulimit: the descriptor limit this process raises at startup.
const (
	UlimitReadFailed       ID = "ulimit.read_failed"
	UlimitCurrent          ID = "ulimit.current"
	UlimitMaxLow           ID = "ulimit.max_low"
	UlimitChangeNotNeeded  ID = "ulimit.change_not_needed"
	UlimitRaiseNotPossible ID = "ulimit.raise_not_possible"
	UlimitChangeFailed     ID = "ulimit.change_failed"
	UlimitChanged          ID = "ulimit.changed"
	UlimitStillLow         ID = "ulimit.still_low"
	UlimitNotApplicable    ID = "ulimit.not_applicable"
)

// startup: what the process checks and reports as it comes up.
const (
	StartupNotRoot  ID = "startup.not_root"
	StartupStarting ID = "startup.starting"
)

// logging: the log file this process writes and the Logs screen reads.
const (
	LoggingToFileDisabled ID = "logging.to_file_disabled"
	LoggingInitFailed     ID = "logging.init_failed"
	LoggingFileReadFailed ID = "logging.file_read_failed"
	// The three below are the press on the Logs screen that empties the file.
	// The refused one is here rather than with the account lines because what
	// it guards is this file, and a reader looking into why the log begins
	// where it does finds all three under the same area.
	LoggingFileClearPasswordWrong ID = "logging.file_clear_password_wrong"
	LoggingFileCleared            ID = "logging.file_cleared"
	LoggingFileClearFailed        ID = "logging.file_clear_failed"
)

// update: reading what the newest release is, and installing it.
const (
	UpdateChecked              ID = "update.checked"
	UpdateCheckFailed          ID = "update.check_failed"
	UpdateCheckLoopStarted     ID = "update.check_loop_started"
	UpdateAutoInstallStarting  ID = "update.auto_install_starting"
	UpdateInstallAsked         ID = "update.install_asked"
	UpdateInstallPasswordWrong ID = "update.install_password_wrong"
	UpdateInstallStartFailed   ID = "update.install_start_failed"
)

// encryption: the key the stored secrets are encrypted with.
const (
	EncryptionKeyCheckHostsReadFailed   ID = "encryption.key_check_hosts_read_failed"
	EncryptionKeyOpensNoStoredPassword  ID = "encryption.key_opens_no_stored_password"
	EncryptionStoredPasswordDoesNotOpen ID = "encryption.stored_password_does_not_open"
	EncryptionKeyOpensStoredPasswords   ID = "encryption.key_opens_stored_passwords"
	EncryptionNoStoredPasswordEncrypted ID = "encryption.no_stored_password_encrypted"
	EncryptionKeyLoadFailed             ID = "encryption.key_load_failed"
	EncryptionInitFailed                ID = "encryption.init_failed"
	EncryptionKeyLoaded                 ID = "encryption.key_loaded"
	EncryptionKeyFileNarrowed           ID = "encryption.key_file_narrowed"
	EncryptionKeyFileNarrowFailed       ID = "encryption.key_file_narrow_failed"
)

// account: the one account that opens the screens, and the sessions opened on it.
const (
	AccountInitialSetupFailed              ID = "account.initial_setup_failed"
	AccountCreatedWithInitialPassword      ID = "account.created_with_initial_password"
	AccountAlreadySetUp                    ID = "account.already_set_up"
	AccountReadFailed                      ID = "account.read_failed"
	AccountSessionCreateFailed             ID = "account.session_create_failed"
	AccountPasswordHashFailed              ID = "account.password_hash_failed"
	AccountSetupNoAccountOnContext         ID = "account.setup_no_account_on_context"
	AccountSetupFailed                     ID = "account.setup_failed"
	AccountSetupCommitFailed               ID = "account.setup_commit_failed"
	AccountInitialPasswordFileRemoveFailed ID = "account.initial_password_file_remove_failed"
	AccountInitialPasswordFileNarrowed     ID = "account.initial_password_file_narrowed"
	AccountInitialPasswordFileNarrowFailed ID = "account.initial_password_file_narrow_failed"
	AccountReadNoAccountOnContext          ID = "account.read_no_account_on_context"
	AccountChangeNoAccountOnContext        ID = "account.change_no_account_on_context"
	AccountChangePasswordWrong             ID = "account.change_password_wrong"
	AccountStoreFailed                     ID = "account.store_failed"
	AccountCredentialsChanged              ID = "account.credentials_changed"
)

// settings: the stored settings, read as the process comes up and changed on the Settings screen.
const (
	SettingsResetFailed              ID = "settings.reset_failed"
	SettingsAlreadyAtDefault         ID = "settings.already_at_default"
	SettingsMissingSettingStored     ID = "settings.missing_setting_stored"
	SettingsSettingPutBackToDefault  ID = "settings.setting_put_back_to_default"
	SettingsReadFailed               ID = "settings.read_failed"
	SettingsRead                     ID = "settings.read"
	SettingsStoreFailed              ID = "settings.store_failed"
	SettingsLogLevelUnknown          ID = "settings.log_level_unknown"
	SettingsDefaultPortHeldByForward ID = "settings.default_port_held_by_forward"
	SettingsDefaultPortHeldBySocks   ID = "settings.default_port_held_by_socks"
)

// certificate: the TLS certificate this installation is served under.
const (
	CertificatePrepareFailed    ID = "certificate.prepare_failed"
	CertificateGenerated        ID = "certificate.generated"
	CertificateReadFromDatabase ID = "certificate.read_from_database"
	CertificateServedUnparsable ID = "certificate.served_unparsable"
	CertificateCreateFailed     ID = "certificate.create_failed"
	CertificateStoreFailed      ID = "certificate.store_failed"
	CertificateNewUnparsable    ID = "certificate.new_unparsable"
	CertificateReplaced         ID = "certificate.replaced"
	CertificateNotUsableYet     ID = "certificate.not_usable_yet"
)

// api_server: the server that answers the API and the web UI.
const (
	ApiServerRedirectServerStopped  ID = "api_server.redirect_server_stopped"
	ApiServerServingHttps           ID = "api_server.serving_https"
	ApiServerServingPlain           ID = "api_server.serving_plain"
	ApiServerStartFailed            ID = "api_server.start_failed"
	ApiServerShutdownFailed         ID = "api_server.shutdown_failed"
	ApiServerRedirectShutdownFailed ID = "api_server.redirect_shutdown_failed"
	ApiServerPortCloseFailed        ID = "api_server.port_close_failed"
	ApiServerPortTakenFallback      ID = "api_server.port_taken_fallback"
)

// shutdown: what the process reports as it goes down.
const (
	ShutdownSignalReceived  ID = "shutdown.signal_received"
	ShutdownUninstalled     ID = "shutdown.uninstalled"
	ShutdownRestartAsked    ID = "shutdown.restart_asked"
	ShutdownStoppingTunnels ID = "shutdown.stopping_tunnels"
	ShutdownExiting         ID = "shutdown.exiting"
)

// restart: the restart that ends this process and runs this program again.
const (
	RestartEndsWithThisProcess ID = "restart.ends_with_this_process"
	RestartRunningAgain        ID = "restart.running_again"
	RestartExecFailed          ID = "restart.exec_failed"
	RestartAsked               ID = "restart.asked"
	RestartAnswerWriteFailed   ID = "restart.answer_write_failed"
)

// host: the Hosts the screens add, change and remove.
const (
	HostPrivateKeyEncryptFailed            ID = "host.private_key_encrypt_failed"
	HostPasswordEncryptFailed              ID = "host.password_encrypt_failed"
	HostCreateFailed                       ID = "host.create_failed"
	HostNextNumberReadFailed               ID = "host.next_number_read_failed"
	HostNewServicePortsReadFailed          ID = "host.new_service_ports_read_failed"
	HostNewServicePortAssignFailed         ID = "host.new_service_port_assign_failed"
	HostCountFailed                        ID = "host.count_failed"
	HostListFetchFailed                    ID = "host.list_fetch_failed"
	HostFetchFailed                        ID = "host.fetch_failed"
	HostUpdateFailed                       ID = "host.update_failed"
	HostDeleteFailed                       ID = "host.delete_failed"
	HostServicePortAssignmentsDeleteFailed ID = "host.service_port_assignments_delete_failed"
	HostServicePortAssignmentsFetchFailed  ID = "host.service_port_assignments_fetch_failed"
	HostServicePortAssignFailed            ID = "host.service_port_assign_failed"
	HostServicePortsRemoveFailed           ID = "host.service_ports_remove_failed"
	// The two lines the host key approval writes. Which SSH server a Host
	// trusts is what keeps its password from being offered to somebody else,
	// so a change of it is recorded whether it went through or not: the line
	// carries the fingerprint, which is a digest of a public key and not a
	// secret, and never the password that was asked for.
	HostHostKeyApproved              ID = "host.host_key_approved"
	HostHostKeyApprovalPasswordWrong ID = "host.host_key_approval_password_wrong"
)

// service_port: the service ports the screens add, change and remove.
const (
	ServicePortCreateFailed                ID = "service_port.create_failed"
	ServicePortHostsReadFailed             ID = "service_port.hosts_read_failed"
	ServicePortHostAssignFailed            ID = "service_port.host_assign_failed"
	ServicePortCountFailed                 ID = "service_port.count_failed"
	ServicePortListFetchFailed             ID = "service_port.list_fetch_failed"
	ServicePortFetchFailed                 ID = "service_port.fetch_failed"
	ServicePortUpdateFailed                ID = "service_port.update_failed"
	ServicePortDeleteFailed                ID = "service_port.delete_failed"
	ServicePortHostAssignmentsDeleteFailed ID = "service_port.host_assignments_delete_failed"
)

// local_forward: the local forwards the screens add, change and remove.
const (
	LocalForwardCreateFailed    ID = "local_forward.create_failed"
	LocalForwardListFetchFailed ID = "local_forward.list_fetch_failed"
	LocalForwardFetchFailed     ID = "local_forward.fetch_failed"
	LocalForwardUpdateFailed    ID = "local_forward.update_failed"
	LocalForwardDeleteFailed    ID = "local_forward.delete_failed"
)

// status: what the Status screen asks about the tunnels.
const (
	StatusTunnelsToRunCountFailed     ID = "status.tunnels_to_run_count_failed"
	StatusTunnelRowsCountFailed       ID = "status.tunnel_rows_count_failed"
	StatusConnectedTunnelsCountFailed ID = "status.connected_tunnels_count_failed"
	StatusTunnelsFetchFailed          ID = "status.tunnels_fetch_failed"
	StatusHostTunnelsFetchFailed      ID = "status.host_tunnels_fetch_failed"
)

// transfer: the export and the import of the tunnel configuration and of the settings.
const (
	TransferExportedFileOpenFailed            ID = "transfer.exported_file_open_failed"
	TransferHostsReadFailed                   ID = "transfer.hosts_read_failed"
	TransferServicePortsReadFailed            ID = "transfer.service_ports_read_failed"
	TransferAssignmentsReadFailed             ID = "transfer.assignments_read_failed"
	TransferLocalForwardsReadFailed           ID = "transfer.local_forwards_read_failed"
	TransferHostSecretDoesNotOpen             ID = "transfer.host_secret_does_not_open"
	TransferExportSealFailed                  ID = "transfer.export_seal_failed"
	TransferExported                          ID = "transfer.exported"
	TransferImported                          ID = "transfer.imported"
	TransferHostLookupFailed                  ID = "transfer.host_lookup_failed"
	TransferHostPasswordEncryptFailed         ID = "transfer.host_password_encrypt_failed"
	TransferHostPrivateKeyEncryptFailed       ID = "transfer.host_private_key_encrypt_failed"
	TransferHostReplaceFailed                 ID = "transfer.host_replace_failed"
	TransferHostCreateFailed                  ID = "transfer.host_create_failed"
	TransferServicePortLookupFailed           ID = "transfer.service_port_lookup_failed"
	TransferServicePortCreateFailed           ID = "transfer.service_port_create_failed"
	TransferServicePortReplaceFailed          ID = "transfer.service_port_replace_failed"
	TransferHostReadBackFailed                ID = "transfer.host_read_back_failed"
	TransferHostAssignmentsClearFailed        ID = "transfer.host_assignments_clear_failed"
	TransferAssignmentServicePortLookupFailed ID = "transfer.assignment_service_port_lookup_failed"
	TransferAssignmentStoreFailed             ID = "transfer.assignment_store_failed"
	TransferLocalForwardLookupFailed          ID = "transfer.local_forward_lookup_failed"
	TransferHostLocalForwardsClearFailed      ID = "transfer.host_local_forwards_clear_failed"
	TransferLocalForwardStoreFailed           ID = "transfer.local_forward_store_failed"
	TransferSettingsReadForExportFailed       ID = "transfer.settings_read_for_export_failed"
	TransferSettingsSealFailed                ID = "transfer.settings_seal_failed"
	TransferSettingsExported                  ID = "transfer.settings_exported"
	TransferSettingsReadForImportFailed       ID = "transfer.settings_read_for_import_failed"
	TransferSettingsStoreFailed               ID = "transfer.settings_store_failed"
	TransferSettingsImported                  ID = "transfer.settings_imported"
)

// uninstall: the uninstall that stops the tunnels and removes what was installed.
const (
	UninstallNoAccountOnContext        ID = "uninstall.no_account_on_context"
	UninstallPasswordWrong             ID = "uninstall.password_wrong"
	UninstallStarting                  ID = "uninstall.starting"
	UninstallReconcileLoopStopped      ID = "uninstall.reconcile_loop_stopped"
	UninstallTunnelsStopped            ID = "uninstall.tunnels_stopped"
	UninstallAnswerWriteFailed         ID = "uninstall.answer_write_failed"
	UninstallDatabaseHandleUnreachable ID = "uninstall.database_handle_unreachable"
	UninstallDatabaseCloseFailed       ID = "uninstall.database_close_failed"
	UninstallDatabaseClosed            ID = "uninstall.database_closed"
	UninstallFileRemoved               ID = "uninstall.file_removed"
	UninstallFileNotThere              ID = "uninstall.file_not_there"
	UninstallFileRemoveFailed          ID = "uninstall.file_remove_failed"
	UninstallLogDirectoryReadFailed    ID = "uninstall.log_directory_read_failed"
)
