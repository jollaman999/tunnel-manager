package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/tunnel"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// hostView is how a Host leaves this process, and every answer that carries a
// Host carries one of these rather than the row itself.
//
// The reason there is a view at all is the two host keys. The row holds each
// of them as "<algorithm> <base64>", which is a line of a few hundred
// characters that nobody reads to the end, and what is put in front of an
// operator is the SHA256 fingerprint of it: that is what ssh(1) prints on a
// first connection and what ssh-keygen -lf reports for the key file on the
// server, so it is the one form of the key that can be compared against the
// machine itself. Neither key is a secret, so this is about what can be
// checked rather than about what may be seen.
//
// Writing the fields out rather than embedding models.Host is what keeps the
// sealed password, the private key and its passphrase out of an answer by
// construction: a field added to the row reaches no client until it is added
// here as well. TestTheHostViewCarriesEveryFieldOfAHost is what says the
// leaving out was meant.
type hostView struct {
	ID   uint   `json:"id"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	User string `json:"user"`
	// HostKeyFingerprint is the fingerprint of the key this Host is trusted
	// on, and is empty for one that has never been approved.
	//
	// PendingHostKeyFingerprint is the fingerprint of the key the SSH server
	// presented on a connection that was refused, and is empty when there is
	// nothing waiting. The two are next to each other because the question the
	// operator answers is a comparison of them: a Host that carries both was
	// presented a key other than the one it is trusted on.
	HostKeyFingerprint        string    `json:"host_key_fingerprint"`
	PendingHostKeyFingerprint string    `json:"pending_host_key_fingerprint"`
	Description               string    `json:"description"`
	Enabled                   bool      `json:"enabled"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

// hostViewOf turns a row into what goes out.
func hostViewOf(host models.Host) hostView {
	return hostView{
		ID:                        host.ID,
		IP:                        host.IP,
		Port:                      host.Port,
		User:                      host.User,
		HostKeyFingerprint:        tunnel.HostKeyFingerprint(host.HostKey),
		PendingHostKeyFingerprint: tunnel.HostKeyFingerprint(host.PendingHostKey),
		Description:               host.Description,
		Enabled:                   host.Enabled,
		CreatedAt:                 host.CreatedAt,
		UpdatedAt:                 host.UpdatedAt,
	}
}

// hostViewsOf does the same for a page of them. A page that holds no row is an
// empty slice and not nil, for the reason ListHosts states: a client draws a
// list out of it, and null is not a list.
func hostViewsOf(hosts []models.Host) []hostView {
	views := make([]hostView, 0, len(hosts))
	for _, host := range hosts {
		views = append(views, hostViewOf(host))
	}

	return views
}

// hostKeyApproval is what the approval is asked with.
//
// Fingerprint is the fingerprint the operator compared against the server and
// is saying yes to, and the approval goes through only while it is still the
// one the row holds. The screen is drawn from an answer that was read at some
// earlier moment, and a Host whose server presented another key in between
// would otherwise have that later key approved by a click meant for the one on
// the screen. Sending it back is what ties the two together.
//
// Password is the password of this account. It is read only where the Host
// already carries a trusted key, which is the case where the approval replaces
// one: a session left open on an unattended screen is otherwise one click away
// from trusting whatever is answering in place of the server. A Host that
// carries no key yet is approved on the session alone, because there is no
// trust to overturn and because that click is on the path of registering every
// Host.
type hostKeyApproval struct {
	Fingerprint string `json:"fingerprint"`
	Password    string `json:"password"`
}

// ApproveHostKey makes the key the SSH server presented the key this Host is
// trusted on.
//
// It is the one way a key becomes trusted. Nothing this end can see says that
// a key is the right one, so the connection that met it wrote it down under
// PendingHostKey and refused (tunnel.Manager.hostKeyCallback), and what
// happens here is a person saying that they compared the fingerprint with the
// server. There is no call that refuses a key on purpose: a key that is not
// approved is already refused, and the Host stays where it is.
func (h *Handler) ApproveHostKey(c echo.Context) error {
	id, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil {
		return failure(c, http.StatusBadRequest, errHostIDInvalid, errorArgs{"reason": err.Error()})
	}

	var req hostKeyApproval

	err = c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	// The password of the account is checked here, before the transaction is
	// opened, and the row above it is read for no other reason than to find
	// out whether it has to be.
	//
	// It reads as something that belongs inside the transaction with the rest
	// of the checks, and it must not be moved there. The database is run on a
	// single connection (database.New says why), so an open transaction holds
	// the only connection there is, and accountPasswordRefused reads the
	// account through the default handle: inside the transaction that read
	// waits for a connection this same request is holding, forever, and every
	// later request that touches the database waits behind it. Even where
	// there were connections to spare, checking a password is a bcrypt
	// comparison of some hundred milliseconds, which is not time to hold a
	// transaction open for.
	//
	// The two checks are not the same kind of thing, which is what makes the
	// split hold. The password answers whether this person may approve
	// anything at all, and needs nothing of the row; the fingerprint answers
	// what is being approved, and is compared against the row inside the
	// transaction the write goes into, which is where it has to stay.
	var refused *refusal

	passwordWasChecked := false

	var current models.Host

	err = h.db.First(&current, id).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	// A Host that is not there at all is left to the read inside the
	// transaction, which is what answers with the not found.
	if err == nil && current.HostKey != "" {
		refused = h.accountPasswordRefused(c, req.Password)
		passwordWasChecked = true
	}

	tx := h.db.Begin()

	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	// The row is read inside the transaction the write goes into, so that what
	// is approved is what was checked. See UpdateHost for what that rests on.
	var host models.Host

	err = tx.First(&host, id).Error
	if err != nil {
		tx.Rollback()

		if errors.Is(err, gorm.ErrRecordNotFound) {
			return failure(c, http.StatusNotFound, errHostNotFound)
		}

		h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errHostFetchFailed)
	}

	waiting := tunnel.HostKeyFingerprint(host.PendingHostKey)
	if waiting == "" {
		tx.Rollback()

		return failure(c, http.StatusConflict, errHostKeyNothingToApprove)
	}

	// The comparison is byte for byte and not case insensitive: a fingerprint
	// is a digest written in base64, where a letter of the other case is
	// another digest.
	if strings.TrimSpace(req.Fingerprint) != waiting {
		tx.Rollback()

		return failure(c, http.StatusConflict, errHostKeyFingerprintChanged, errorArgs{"waiting": waiting})
	}

	if host.HostKey != "" {
		// The trusted key the password was decided on was read before the
		// transaction, and a Host that has gained one since is a Host whose
		// approval would go through a check that was never made. It is
		// refused under the same code as a password that does not open the
		// account, because that is what the client has to do about it: send
		// one. Nothing else reaches here, since a trusted key is only ever
		// cleared by a Host being deleted.
		if !passwordWasChecked {
			refused = refuse(http.StatusUnauthorized, errHostKeyPasswordWrong)
		}

		if refused != nil {
			// Nothing has been written at this point: the trusted key of the
			// Host is left as it is and the key that was presented stays
			// where it is, waiting for an approval that opens the account.
			tx.Rollback()

			// A password that does not open the account is written down,
			// because this is the one call where a password stands between a
			// session and the trust of a Host: somebody working through a
			// session that is not theirs leaves nothing else behind. The
			// other refusals accountPasswordRefused can answer with are
			// logged where they are raised, so only this one is written here,
			// and the password that was sent is on no line of it.
			if refused.code == errHostKeyPasswordWrong {
				h.logger.Warn("a host key approval was refused: the password does not open the account",
					logid.HostHostKeyApprovalPasswordWrong.Field(), zap.Uint("host_id", host.ID))
			}

			return refused.answer(c)
		}
	}

	// The two columns move together. A row left holding the same key under
	// both would show the key that is trusted as one that is waiting to be
	// approved, and the screen would go on asking about a question that has
	// been answered.
	host.HostKey = host.PendingHostKey
	host.PendingHostKey = ""

	err = tx.Save(&host).Error
	if err != nil {
		tx.Rollback()
		h.logger.Error("failed to update Host", logid.HostUpdateFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errHostUpdateFailed)
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	// What this Host is trusted on from here on. It is recorded because the
	// approval is what decides whether the SSH password of the Host is
	// offered to the server or to whoever is answering in its place, and
	// nothing else in the log says that the answer was given. The fingerprint
	// is the digest of a public key and is no secret; the password that may
	// have been asked for above is on no line.
	h.logger.Info("the host key of the Host was approved",
		logid.HostHostKeyApproved.Field(),
		zap.Uint("host_id", host.ID),
		zap.String("fingerprint", waiting))

	// The tunnels of this Host were refused against the key the Host carried
	// when they were built, which was no key or the old one, and a refused
	// tunnel has no connection left to bring down. The trusted key is part of
	// what a tunnel is built from (tunnel.connectionFingerprint), so a
	// reconcile pass sees a Host that no longer matches what is running and
	// builds it again. Without this wake the approval would take hold at the
	// next pass of the loop rather than now.
	h.manager.WakeReconcile()

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    hostViewOf(host),
	})
}

// accountPasswordRefused checks the password of the account this session
// belongs to and returns what to answer with when it does not open it, or nil
// when it does.
//
// It reads the account behind the session rather than a username in the body,
// the way the uninstall does: what is being asked for is that the person at
// the screen is the one who logged in, and a request that named the account
// would let a stolen session name any of them.
//
// It reads through the default handle and is not to be called from inside a
// transaction. The pool holds one connection, so the read would wait for the
// connection the transaction is holding and never be given it. ApproveHostKey
// says the rest of it.
func (h *Handler) accountPasswordRefused(c echo.Context, password string) *refusal {
	userID, ok := c.Get(contextUserIDKey).(uint)
	if !ok {
		// The middleware is what puts it there, so getting here means the
		// route was hung somewhere the middleware does not cover.
		h.logger.Error("a call that asks for the password of the account was reached with no account "+
			"on the context", logid.AccountReadNoAccountOnContext.Field())

		return refuse(http.StatusInternalServerError, errAccountReadFailed)
	}

	var user models.User

	err := h.db.First(&user, userID).Error
	if err != nil {
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))

		return refuse(http.StatusInternalServerError, errAccountReadFailed)
	}

	if !auth.CheckPassword(user.PasswordHash, password) {
		return refuse(http.StatusUnauthorized, errHostKeyPasswordWrong)
	}

	return nil
}
