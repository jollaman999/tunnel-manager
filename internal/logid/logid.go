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
	TunnelWildcardLocalAddress        ID = "tunnel.wildcard_local_address"
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
)

// database: opening the database and what the queries report.
const (
	DatabaseGormMessage        ID = "database.gorm_message"
	DatabaseQueryFailed        ID = "database.query_failed"
	DatabaseQuerySlow          ID = "database.query_slow"
	DatabaseQuery              ID = "database.query"
	DatabaseServicePortsFilled ID = "database.service_ports_filled"
	DatabaseOpened             ID = "database.opened"
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
	StartupNotRoot ID = "startup.not_root"
)
