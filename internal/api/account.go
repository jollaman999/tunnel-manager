package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/jollaman999/tunnel-manager/internal/auth"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// accountNothingToChangeMessage is the answer to a request that names neither
// of the two values it could change. There is nothing to do for it and nothing
// to guess at either: a request that meant to change the username sent one, and
// an empty new password is not a password the account would ever take.
const accountNothingToChangeMessage = "Name a username, a new password or both. " +
	"A request that names neither changes nothing"

// accountWrongPasswordMessage is the answer to a change that did not bring the
// password the account is open with. It is worded the way the uninstall words
// the same refusal, because it is the same question being put: the session is
// already in, and what is being asked for is the password again.
//
// It says which of the values was refused on purpose. The login says nothing of
// the sort, because there the caller is a stranger; here the caller holds a
// session of this account, and every one of the refusals below tells them
// something about it already.
const accountWrongPasswordMessage = "The current password does not open this account"

// accountRequest is what the Settings screen sends to change the credentials.
//
// The two values that can change are sent only when they are changing, so a
// request that renames the account leaves the password alone by saying nothing
// about it. The current password is not one of those and comes with every
// request, the username-only change included: what this call is for is the case
// where somebody else may know the credentials, and a session left open on an
// unattended screen is otherwise all it takes.
//
// There is no field for the second copy of the new password. The screen asks
// for it twice to catch a typo, and a server handed the same string twice
// learns nothing from the second one: the check belongs where the typing
// happens.
type accountRequest struct {
	CurrentPassword string `json:"current_password"`
	Username        string `json:"username"`
	NewPassword     string `json:"new_password"`
}

// accountView is the account as a screen draws it. The username is the whole of
// it: the hash never leaves the process, and the flag that says whether the
// setup is done is not something a session that got this far can still be in.
type accountView struct {
	Username string `json:"username"`
}

// accountChanged is what a change answers with.
//
// Which of the two changed is carried apart from the username itself, because a
// request that sent only a password gets the unchanged name back and the screen
// would otherwise report it as a rename.
//
// SessionsEnded is the half of this call that happens away from the operator.
// Every other session is gone, on this browser and on any other, and a screen
// that did not say so would leave them to find out at the next device they pick
// up.
type accountChanged struct {
	Username        string `json:"username"`
	UsernameChanged bool   `json:"username_changed"`
	PasswordChanged bool   `json:"password_changed"`
	SessionsEnded   int    `json:"sessions_ended"`
}

// GetAccount answers with what the account is called now.
//
// The screen shows it next to the box that replaces it. A box that is empty
// until something is typed into it does not say what it is replacing, and the
// username is not written down anywhere the page can read after a reload: the
// session cookie carries a token and nothing else.
func (h *AuthHandler) GetAccount(c echo.Context) error {
	userID, ok := c.Get(contextUserIDKey).(uint)
	if !ok {
		// The middleware is what puts it there, so getting here means the route
		// was hung somewhere the middleware does not cover.
		h.logger.Error("the account read was reached with no account on the context",
			logid.AccountReadNoAccountOnContext.Field())
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	user, err := h.readUser(userID)
	if err != nil {
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    accountView{Username: user.Username},
	})
}

// ChangeAccount replaces the username, the password or both, and ends every
// session but the one that asked for it.
//
// The order is what this is made of. Nothing about the account is read before
// the shape of the request has been looked at, nothing the account holds is
// compared against before the current password has been proved, and no session
// is dropped before the row has been written: a change that failed to store
// must not be one that logged everybody out.
func (h *AuthHandler) ChangeAccount(c echo.Context) error {
	var req accountRequest

	err := c.Bind(&req)
	if err != nil {
		// What went wrong with the parse is not passed on. It says nothing the
		// caller can act on beyond what naming the fields says, and the body it
		// failed to read is the one carrying the password.
		return failure(c, http.StatusBadRequest, errAccountRequestInvalid)
	}

	// A value that is not sent is a value that stays as it is. An empty new
	// password cannot be read as anything else: the shortest one the account
	// takes is minPasswordBytes long.
	username := strings.TrimSpace(req.Username)
	changeUsername := req.Username != ""
	changePassword := req.NewPassword != ""

	if !changeUsername && !changePassword {
		return failure(c, http.StatusBadRequest, errAccountNothingToChange)
	}

	// The name is stored with the surrounding space taken off, the way the
	// setup stores it, because that is how the login compares it: a name stored
	// with a trailing space is one the operator cannot type back in. A name
	// made of nothing but space is a name being cleared rather than one being
	// left alone, and the account cannot be left without one.
	if changeUsername && username == "" {
		return failure(c, http.StatusBadRequest, errAccountUsernameEmpty)
	}

	// The bounds are the ones the setup holds, not a second pair. A password
	// changed here is checked by the same login, so a rule that differed would
	// let a password in through one door that the other would not have taken.
	// len on a string counts bytes, which is the unit bcrypt reads it in.
	if changePassword {
		switch {
		case len(req.NewPassword) < minPasswordBytes:
			return failure(c, http.StatusBadRequest, errAccountNewPasswordShort, errorArgs{"min": strconv.Itoa(minPasswordBytes)})
		case len(req.NewPassword) > maxPasswordBytes:
			return failure(c, http.StatusBadRequest, errAccountNewPasswordLong, errorArgs{"max": strconv.Itoa(maxPasswordBytes)})
		}
	}

	userID, ok := c.Get(contextUserIDKey).(uint)
	if !ok {
		// The middleware is what puts it there, so getting here means the route
		// was hung somewhere the middleware does not cover.
		h.logger.Error("the account change was reached with no account on the context",
			logid.AccountChangeNoAccountOnContext.Field())
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	user, err := h.readUser(userID)
	if err != nil {
		h.logger.Error("failed to read the account", logid.AccountReadFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountReadFailed)
	}

	if !auth.CheckPassword(user.PasswordHash, req.CurrentPassword) {
		// Nothing has been touched at this point and the log says so, the way
		// the uninstall says it: a refusal has to be told from a change that
		// ran. Neither password is in the line. The log goes to the console as
		// well as to a file that is kept and rotated, so a credential written
		// there outlives every screen it was typed on.
		h.logger.Warn("the account was asked to be changed with a password that does not open "+
			"it, so nothing was changed and no session was ended",
			logid.AccountChangePasswordWrong.Field())

		return failure(c, http.StatusUnauthorized, errAccountPasswordWrong)
	}

	// A value that is already the stored one is refused rather than stored
	// again. What this call does past the write is end every other session, on
	// the grounds that the credentials those sessions were opened with are not
	// the credentials any more. Writing the same name back, or hashing the same
	// password afresh, leaves that untrue, and the answer would report a change
	// nobody could act on.
	//
	// It is checked here and not above, because answering whether a string is
	// the password of the account is answering a guess at it. Above this line
	// the request has not proved it may hear that.
	if changeUsername && username == user.Username {
		return failure(c, http.StatusBadRequest, errAccountUsernameUnchanged)
	}

	if changePassword && auth.CheckPassword(user.PasswordHash, req.NewPassword) {
		return failure(c, http.StatusBadRequest, errAccountPasswordUnchanged)
	}

	if changeUsername {
		user.Username = username
	}

	if changePassword {
		hash, hashErr := auth.HashPassword(req.NewPassword)
		if hashErr != nil {
			h.logger.Error("failed to hash the password", logid.AccountPasswordHashFailed.Field(), zap.Error(hashErr))
			return failure(c, http.StatusInternalServerError, errAccountHashFailed)
		}

		user.PasswordHash = hash
	}

	err = h.db.Save(user).Error
	if err != nil {
		h.logger.Error("failed to store the changed account", logid.AccountStoreFailed.Field(), zap.Error(err))
		return failure(c, http.StatusInternalServerError, errAccountStoreFailed)
	}

	// Everything below this point runs only because the row has been written.
	// Sessions dropped ahead of a write that then failed would be sessions
	// ended for a change that never happened.
	token := ""

	cookie, err := c.Cookie(sessionCookieName)
	if err == nil {
		token = cookie.Value
	}

	// Every other session goes, and the one that asked for this stays. That is
	// one rule for both values rather than one for the password and another for
	// the name: half the reason to change either is that somebody else may know
	// them, and a session that is already open is held by whoever is holding it
	// whatever the account is called now. Keeping this one protects nothing to
	// take away: it belongs to the operator who just chose the new credentials,
	// and throwing them out would leave them unable to read what happened.
	//
	// The session that stays keeps its tokens, the CSRF one included. That
	// token is not a credential on its own: it is only taken next to the
	// session it was made for, and that session is the one being kept. Handing
	// out a new one would have to reach a page that is in the middle of this
	// request, and a page that missed it could change nothing until a later
	// answer set the cookie again.
	ended := h.sessions.DeleteAllExcept(token)

	// Which values changed is logged, and neither value is. The password is
	// kept out for the reason above; the username is kept out because it is one
	// half of what opens this account, which is why the login refuses to say
	// which half of a guess was right.
	h.logger.Info("the credentials of the account were changed, so every other session was ended",
		logid.AccountCredentialsChanged.Field(),
		zap.Bool("username_changed", changeUsername),
		zap.Bool("password_changed", changePassword),
		zap.Int("sessions_ended", ended))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data: accountChanged{
			Username:        user.Username,
			UsernameChanged: changeUsername,
			PasswordChanged: changePassword,
			SessionsEnded:   ended,
		},
	})
}
