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

// hostKeysWaiting narrows a read to the Hosts whose SSH server presented a key
// that nobody has answered for yet.
//
// The two columns are read through COALESCE. They were added to installations
// whose rows were written before they existed, and AutoMigrate fills those with
// NULL (models.Host says so), and in SQL a NULL compared with the empty string
// is neither equal nor unequal but NULL, which no WHERE keeps: a comparison
// written the plain way would leave every one of those rows out. That is
// exactly the set an upgrade leaves waiting, which is the set this is for.
func hostKeysWaiting(db *gorm.DB) *gorm.DB {
	return db.Model(&models.Host{}).Where("COALESCE(pending_host_key, '') <> ''")
}

// hostKeysFirstApproval is the Hosts of that set that have never been approved,
// and hostKeysChanged the ones that are trusted on some other key.
//
// The two are counted and listed apart because they are not the same question.
// A Host that carries no key yet is on the path of registering it, and an
// upgrade leaves every Host there at once; a Host whose server presented a key
// other than the one it is trusted on is either a rebuilt server or a
// connection that is not reaching the server at all, and there is never a
// reason for a screen to hand those two the same word or the same colour.
func hostKeysFirstApproval(db *gorm.DB) *gorm.DB {
	return hostKeysWaiting(db).Where("COALESCE(host_key, '') = ''")
}

func hostKeysChanged(db *gorm.DB) *gorm.DB {
	return hostKeysWaiting(db).Where("COALESCE(host_key, '') <> ''")
}

// hostKeyWaiting is one Host of the list of the ones waiting to be approved.
//
// It is not a hostView. What the list is read for is the comparison, so what it
// carries is the Host named the way an operator names it and the fingerprints
// the comparison is made of, and nothing else of the row: the panel puts a
// hundred of these on a screen at a time, and the port, the user, the
// description and the two timestamps are a hundred rows of what nobody is
// looking at there.
//
// Mismatch is what tells the two states apart. It is carried rather than left
// to a client comparing TrustedFingerprint with the empty string, because it is
// what decides whether a row may be ticked in bulk and whether the approval
// asks for the password of the account, and neither of those is a thing to work
// out twice.
//
// The keys themselves are not in it, for the reason hostView states.
type hostKeyWaiting struct {
	HostID             uint   `json:"host_id"`
	IP                 string `json:"ip"`
	Mismatch           bool   `json:"mismatch"`
	Fingerprint        string `json:"fingerprint"`
	TrustedFingerprint string `json:"trusted_fingerprint"`
}

// hostKeyWaitingOf turns a row into one of those.
func hostKeyWaitingOf(host models.Host) hostKeyWaiting {
	trusted := tunnel.HostKeyFingerprint(host.HostKey)

	return hostKeyWaiting{
		HostID:             host.ID,
		IP:                 host.IP,
		Mismatch:           trusted != "",
		Fingerprint:        tunnel.HostKeyFingerprint(host.PendingHostKey),
		TrustedFingerprint: trusted,
	}
}

// ListHostKeysWaiting answers one page of the Hosts that have a key waiting to
// be approved.
//
// The filtering and the paging are both the database's. The status screen
// carries the count of these over the whole installation, and an upgrade puts
// every Host into that count at once, so the list behind it is as long as the
// list of Hosts: an answer holding all of them, for a client to pick the
// waiting ones out of, is the load the paging elsewhere here is there to avoid.
//
// The order is the id, as ListHosts orders, and for the same reason: LIMIT and
// OFFSET cut a page out of an order, so an order the database is not told would
// put one Host on two pages and another on none.
func (h *Handler) ListHostKeysWaiting(c echo.Context) error {
	page, refused := readListPage(c)
	if refused != nil {
		return badListPage(c, refused)
	}

	var total int64

	err := hostKeysWaiting(h.db).Count(&total).Error
	if err != nil {
		h.logger.Error("failed to count the Hosts", logid.HostCountFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errHostListFailed)
	}

	page = page.fitTo(total)

	var hosts []models.Host

	err = hostKeysWaiting(h.db).Order("id").Limit(page.size).Offset(page.offset()).Find(&hosts).Error
	if err != nil {
		h.logger.Error("failed to fetch Hosts", logid.HostListFetchFailed.Field(), zap.Error(err))

		return failure(c, http.StatusInternalServerError, errHostListFailed)
	}

	// An empty page is an array and not null, for the reason ListHosts says.
	items := make([]hostKeyWaiting, 0, len(hosts))
	for _, host := range hosts {
		items = append(items, hostKeyWaitingOf(host))
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: listPageOf{
			Items: items,
			Total: total,
			Page:  page.number,
			Size:  page.size,
		},
	})
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

// hostKeyApprovals is what a bulk approval is asked with: the Hosts it is about
// and, where it needs one, the password of the account.
//
// The password is asked for once for the whole request and not once per Host.
// What it answers is whether the person at the screen is the one who logged in,
// which is one question however many Hosts the press was over, and a bcrypt
// comparison per Host would be a tenth of a second each on a list of two
// hundred.
type hostKeyApprovals struct {
	Hosts    []hostKeyApprovalOf `json:"hosts"`
	Password string              `json:"password"`
}

// hostKeyApprovalOf is one Host of such a request.
//
// Every Host carries its own fingerprint, for the reason hostKeyApproval does:
// the approval goes through only while that is still the key the row holds, so
// a press meant for the keys that were on the screen cannot approve one an SSH
// server presented after they were read. A single fingerprint for the whole
// request would say nothing about any of them.
type hostKeyApprovalOf struct {
	HostID      uint   `json:"host_id"`
	Fingerprint string `json:"fingerprint"`
}

// hostKeyApprovalResult is what became of one Host of such a request.
//
// A Host that was refused carries the refusal the way an answer that says no
// carries one: the code it is named by, the values written into it, and the
// English sentence. A screen says it in the language it is running in and one
// that has never heard of the code shows the English, which is the same thing
// an older screen does with a refusal from a newer server.
//
// Fingerprint is what the Host is trusted on from here on, and is empty for one
// that was refused. It is carried so that a screen can say what it did without
// reading the list it sent back to itself.
type hostKeyApprovalResult struct {
	HostID      uint      `json:"host_id"`
	Approved    bool      `json:"approved"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Error       string    `json:"error,omitempty"`
	Code        errorCode `json:"error_code,omitempty"`
	Args        errorArgs `json:"error_args,omitempty"`
}

// hostKeyApprovalsDone is what the whole of one went out as: the two counts,
// and what became of every Host it named.
type hostKeyApprovalsDone struct {
	Approved int                     `json:"approved"`
	Refused  int                     `json:"refused"`
	Hosts    []hostKeyApprovalResult `json:"hosts"`
}

// hostKeyApprovalRefused is one Host that was not approved.
//
// The refusal is built with refuse rather than written out here, so that these
// go out under the same codes and with the same values as the ones the single
// approval answers with, and so that the check that holds every call site to
// the sentence it raises covers them. The status it is named with is the one
// the same refusal goes out under when it is the whole answer; here it is not,
// because the request as a whole was carried out.
func hostKeyApprovalRefused(id uint, refused *refusal) hostKeyApprovalResult {
	message, _ := renderErrorMessage(errorMessages[refused.code], refused.args)

	return hostKeyApprovalResult{
		HostID: id,
		Error:  message,
		Code:   refused.code,
		Args:   refused.args,
	}
}

// hostKeyApprovalsMax is how many Hosts one bulk approval may name.
//
// It is here for the one connection the database is run on (database.New says
// why). Every Host of a request is read and written inside a single
// transaction, and that transaction holds the only connection there is for as
// long as it runs: every other request that touches the database, and the
// reconcile loop with them, waits behind it. So the size of the request is the
// length of time this server is unavailable for, and it is the caller who
// chooses it.
//
// A thousand is far above what the panel can ask for and far below what the
// body limit lets through. The panel sends the page that is on the screen,
// which is a hundred Hosts at most (pageSizes), so ten pages at once still go
// through untouched and no operator working from a screen meets this; the 1 MB
// this body may be (generalBodyLimit in main.go) holds some ten thousand of
// these entries, which is the request that would hold the connection for
// twenty thousand statements. What is refused is a script handing over its
// whole installation in one press, and what it is told to do is send it in
// several requests, which costs it nothing and leaves the gaps between them
// for everything else to be served in.
const hostKeyApprovalsMax = 1000

// hostKeyApprovalIDsPerRead is how many Host ids go into one IN (...).
//
// A statement carries its values as bound variables, and SQLite prepares no
// statement with more of them than the build takes: 32766 in the builds of
// today and 999 in the ones before 3.32, and a list longer than that comes back
// as an error rather than as rows. Five hundred is below both, so the read is
// the same read wherever this runs, and it is large enough that the list a
// request may name is two statements rather than one. Nothing is gained by
// going nearer either number: the cost of a second read is a statement, and the
// cost of being over the limit is the whole call.
const hostKeyApprovalIDsPerRead = 500

// hostKeysChangedAmong is the Hosts of the given ids that are waiting on a key
// other than the one they are trusted on, which is the set whose approval asks
// for the password of the account.
//
// The read is made in fixed groups rather than in one statement, for the reason
// hostKeyApprovalIDsPerRead states. What the caller gets back is the ids that
// matched, in the order the groups were read, which is the order the ids were
// given in: the callers count them and log them, and neither is an order.
//
// db is the handle to read through, and which one it is handed matters. This is
// called before the transaction is opened and is given the default handle. It
// must not be called with the default handle from inside a transaction: the
// pool holds one connection, the transaction is holding it, and the read would
// wait for it and never be given it, which is what ApproveHostKey sets out at
// length.
func hostKeysChangedAmong(db *gorm.DB, ids []uint) ([]uint, error) {
	changed := make([]uint, 0, len(ids))

	for len(ids) > 0 {
		group := ids
		if len(group) > hostKeyApprovalIDsPerRead {
			group = group[:hostKeyApprovalIDsPerRead]
		}

		var found []uint

		err := hostKeysChanged(db).Where("id IN ?", group).Pluck("id", &found).Error
		if err != nil {
			return nil, err
		}

		changed = append(changed, found...)
		ids = ids[len(group):]
	}

	return changed, nil
}

// ApproveHostKeys approves the keys of several Hosts at once.
//
// It is the same answer ApproveHostKey gives, given over a list, and it exists
// because of what an upgrade looks like: every Host that was registered before
// the host key check existed is waiting for a first approval at the same
// moment, and on an installation of two hundred Hosts that is two hundred
// panels to open. What is compared is still one fingerprint per Host, which is
// why every Host in the request carries its own.
//
// A Host that is refused does not take the rest down with it. The fingerprint
// of one Host says nothing about another, so a key that changed under one of
// them is answered for that one and the rest of the list goes through; what
// each Host met comes back in the answer, because a screen that only knew the
// count would have to read the list again to find out which ones are left.
//
// How many Hosts one request may name is hostKeyApprovalsMax, which says why
// there is a number at all. A request over more than that is refused whole and
// nothing is read for it.
//
// The password is the exception, and it is checked before anything is written:
// a password that does not open the account approves nothing at all. It is the
// one refusal that says nothing about any particular Host, and an approval that
// landed in part on it would leave the operator to work out which of two
// hundred Hosts went through before trying again.
func (h *Handler) ApproveHostKeys(c echo.Context) error {
	var req hostKeyApprovals

	err := c.Bind(&req)
	if err != nil {
		return failure(c, http.StatusBadRequest, errRequestBodyInvalid, errorArgs{"reason": err.Error()})
	}

	// How many Hosts one request may name, for the reason hostKeyApprovalsMax
	// states: the transaction below holds the only connection this server has,
	// so the length of the list is the length of time nothing else is served.
	//
	// It is answered under the code every other refusal about the shape of the
	// body goes out under, rather than one of its own, and it says what is
	// wrong in the sentence: the request is a request this API does not take,
	// which is the same thing a body that fails its validation is, and what the
	// caller does about it is the same too. Nothing here is about any
	// particular Host, so no Host is named and nothing has been read at this
	// point.
	if len(req.Hosts) > hostKeyApprovalsMax {
		return failure(c, http.StatusBadRequest, errRequestValidationFailed, errorArgs{
			"reason": "the request names " + strconv.Itoa(len(req.Hosts)) + " Hosts and at most " +
				strconv.Itoa(hostKeyApprovalsMax) + " may be approved in one request, because they " +
				"are approved in a single transaction that holds the database while it runs. Send " +
				"them in several requests",
		})
	}

	// The password of the account is checked here, before the transaction is
	// opened, for the reason ApproveHostKey states at length: the database is
	// run on a single connection, so an open transaction holds the only
	// connection there is, and accountPasswordRefused reads the account through
	// the default handle. Inside the transaction that read waits for the
	// connection this same request is holding, forever.
	//
	// The read above it is what decides whether the password has to be asked
	// for at all, and it is the same rule the single approval follows: a Host
	// that carries a trusted key is one whose approval replaces it, and the
	// password is what stands between a session left open on an unattended
	// screen and the trust of a Host. A list with none of those in it is
	// approved on the session alone, which is what an upgrade is.
	ids := make([]uint, 0, len(req.Hosts))
	for _, asked := range req.Hosts {
		ids = append(ids, asked.HostID)
	}

	// Read in groups and through the default handle, which is what
	// hostKeysChangedAmong is to be handed and why: the transaction is opened
	// below this, so the one connection is free here, and a list of any length
	// is a number of statements rather than one statement that is too long.
	var trusted []uint

	if len(ids) > 0 {
		trusted, err = hostKeysChangedAmong(h.db, ids)
		if err != nil {
			h.logger.Error("failed to fetch Hosts", logid.HostListFetchFailed.Field(), zap.Error(err))

			return failure(c, http.StatusInternalServerError, errHostListFailed)
		}
	}

	passwordWasChecked := len(trusted) > 0

	if passwordWasChecked {
		refused := h.accountPasswordRefused(c, req.Password)
		if refused != nil {
			// Written down per Host the request was about to replace the
			// trusted key of, and not once for the request, because that is
			// what the line is read for afterwards: which Hosts somebody
			// working through a session that is not theirs was trying to
			// retrust. The password that was sent is on no line of it.
			if refused.code == errHostKeyPasswordWrong {
				for _, id := range trusted {
					h.logger.Warn("a host key approval was refused: the password does not open the account",
						logid.HostHostKeyApprovalPasswordWrong.Field(), zap.Uint("host_id", id))
				}
			}

			return refused.answer(c)
		}
	}

	tx := h.db.Begin()

	err = tx.Error
	if err != nil {
		h.logger.Error("failed to start the transaction", logid.DatabaseTransactionStartFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionBeginFailed)
	}

	results := make([]hostKeyApprovalResult, 0, len(req.Hosts))

	// What was approved, kept to be written to the log once the transaction has
	// been committed. A line written before the commit is a line about a change
	// that a failure to commit would have taken back.
	type approved struct {
		id          uint
		fingerprint string
	}

	var done []approved

	for _, asked := range req.Hosts {
		// Each row is read inside the transaction the write goes into, so that
		// what is approved is what was checked. See UpdateHost for what that
		// rests on.
		var host models.Host

		err = tx.First(&host, asked.HostID).Error
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// A Host that is not there is this one Host's refusal and not
				// the request's: it was deleted while the panel was open, and
				// the others on the screen are still there to be approved.
				results = append(results,
					hostKeyApprovalRefused(asked.HostID, refuse(http.StatusNotFound, errHostNotFound)))

				continue
			}

			tx.Rollback()
			h.logger.Error("failed to fetch Host", logid.HostFetchFailed.Field(), zap.Error(err))

			return failure(c, http.StatusInternalServerError, errHostFetchFailed)
		}

		waiting := tunnel.HostKeyFingerprint(host.PendingHostKey)
		if waiting == "" {
			results = append(results, hostKeyApprovalRefused(host.ID,
				refuse(http.StatusConflict, errHostKeyNothingToApprove)))

			continue
		}

		// Byte for byte and not case insensitive, for the reason
		// ApproveHostKey gives.
		if strings.TrimSpace(asked.Fingerprint) != waiting {
			results = append(results, hostKeyApprovalRefused(host.ID,
				refuse(http.StatusConflict, errHostKeyFingerprintChanged, errorArgs{"waiting": waiting})))

			continue
		}

		// A Host that has gained a trusted key since the read above is a Host
		// whose approval would go through a check that was never made. It is
		// refused under the same code as a password that does not open the
		// account, because that is what the client has to do about it: send
		// one. It is this Host's refusal alone, since the rest of the list was
		// checked against what the read found.
		if host.HostKey != "" && !passwordWasChecked {
			results = append(results, hostKeyApprovalRefused(host.ID,
				refuse(http.StatusUnauthorized, errHostKeyPasswordWrong)))

			continue
		}

		// The two columns move together, for the reason ApproveHostKey gives.
		host.HostKey = host.PendingHostKey
		host.PendingHostKey = ""

		err = tx.Save(&host).Error
		if err != nil {
			tx.Rollback()
			h.logger.Error("failed to update Host", logid.HostUpdateFailed.Field(), zap.Error(err))

			return failure(c, http.StatusInternalServerError, errHostUpdateFailed)
		}

		results = append(results, hostKeyApprovalResult{
			HostID:      host.ID,
			Approved:    true,
			Fingerprint: waiting,
		})

		done = append(done, approved{id: host.ID, fingerprint: waiting})
	}

	err = tx.Commit().Error
	if err != nil {
		h.logger.Error("failed to commit the transaction", logid.DatabaseTransactionCommitFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errTransactionCommitFailed)
	}

	// One line per approval, under the identifier a single approval is written
	// under. What is asked of the log afterwards is which Host was trusted on
	// which key and when, and a line per request would answer that for the
	// first Host of it alone.
	for _, one := range done {
		h.logger.Info("the host key of the Host was approved",
			logid.HostHostKeyApproved.Field(),
			zap.Uint("host_id", one.id),
			zap.String("fingerprint", one.fingerprint))
	}

	// Once, and only where something was written, for the reason
	// UpdateHostServicePorts wakes it that way. The pass that follows sees
	// every Host that was approved, so a wake per Host would be the same pass
	// asked for two hundred times.
	if len(done) > 0 {
		h.manager.WakeReconcile()
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: hostKeyApprovalsDone{
			Approved: len(done),
			Refused:  len(results) - len(done),
			Hosts:    results,
		},
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
