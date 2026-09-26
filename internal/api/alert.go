package api

import (
	"net/http"

	"github.com/jollaman999/tunnel-manager/internal/alert"
	"github.com/jollaman999/tunnel-manager/internal/logid"
	"github.com/jollaman999/tunnel-manager/internal/models"
	"github.com/jollaman999/tunnel-manager/internal/settings"
	"github.com/labstack/echo/v4"
	"go.uber.org/zap"
)

// alertTestSent is what a test that went through answers with.
type alertTestSent struct {
	// Channel is "webhook" or "smtp", whichever the test was sent through.
	Channel string `json:"channel"`
}

// alertTestSettings is the stored settings with what the body of a test names
// put over them, which is what a test is sent under. Nothing is stored: a test
// is how a screen finds out whether the boxes it holds would work before
// anybody saves them, and a test that saved them would be a save nobody
// pressed.
//
// mail is whether the test is the one that sends the mail password. That test
// is held to guardStoredPassword the way a save is, since a test that sent the
// stored password to a server named in the body would hand it to whoever named
// the server. The webhook test sends no password and is not held to it.
func (h *SettingsHandler) alertTestSettings(c echo.Context, mail bool) (*settings.Settings, *refusal) {
	stored, err := settings.LoadOpened(h.db, h.cipher)
	if err != nil {
		h.logger.Error("failed to read the settings", logid.SettingsReadFailed.Field(), zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errSettingsReadFailed)
	}

	var req updateSettingsRequest

	err = c.Bind(&req)
	if err != nil {
		return nil, unreadableBody(err)
	}

	asked := *stored
	req.apply(&asked)

	err = asked.Validate()
	if err != nil {
		return nil, settingsRefusal(err, &asked, errSettingsRefused)
	}

	if mail {
		refused := guardStoredPassword(&req, stored, &asked)
		if refused != nil {
			return nil, refused
		}
	}

	// The password is sealed for the test the way a save seals it, because
	// the sender opens it the way it opens a stored one. The sealed copy goes
	// nowhere but the message this test sends.
	err = req.applyPassword(&asked, h.cipher)
	if err != nil {
		h.logger.Error("failed to encrypt the mail password", logid.SettingsSmtpPasswordSealFailed.Field(), zap.Error(err))
		return nil, refuse(http.StatusInternalServerError, errSettingsSMTPPasswordSeal)
	}

	return &asked, nil
}

// testEvent is the alert a test sends.
func testEvent() alert.Event {
	return alert.Event{
		Event:        alert.EventTest,
		Installation: alert.InstallationName(),
	}
}

// TestAlertWebhook posts a test alert to the webhook.
//
// @Summary      Post a test alert to the webhook
// @Description  The stored settings are used, with any alert setting the body names put over them; nothing is stored. The body is the one PUT /settings takes and may be left out. A webhook that is not reached, or answers outside 2xx, is answered with 502 and what went wrong in error_args.reason.
// @Tags         settings
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.updateSettingsRequest  false  "Settings to test with instead of the stored ones"
// @Success  200  {object}  models.Response{data=api.alertTestSent}
// @Failure  400  {object}  api.errorBody  "A setting broke one of the rules, or no webhook address is set"
// @Failure  502  {object}  api.errorBody  "The webhook was not reached or did not take the alert"
// @Router       /settings/alert/test-webhook [post]
func (h *SettingsHandler) TestAlertWebhook(c echo.Context) error {
	asked, refused := h.alertTestSettings(c, false)
	if refused != nil {
		return refused.answer(c)
	}

	if asked.AlertWebhookURL == "" {
		return failure(c, http.StatusBadRequest, errAlertTestWebhookOff)
	}

	err := h.alerts.Webhook(c.Request().Context(), asked.AlertWebhookURL, testEvent())
	if err != nil {
		h.logger.Warn("a test alert was not delivered",
			logid.AlertTestFailed.Field(),
			zap.String("channel", "webhook"),
			zap.Error(err))

		return failure(c, http.StatusBadGateway, errAlertTestFailed, errorArgs{"reason": err.Error()})
	}

	h.logger.Info("sent a test alert", logid.AlertTestSent.Field(), zap.String("channel", "webhook"))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    alertTestSent{Channel: "webhook"},
	})
}

// TestAlertSMTP sends a test alert by mail.
//
// @Summary      Send a test alert by mail
// @Description  The stored settings are used, with any alert setting the body names put over them, smtp_password among them; nothing is stored. The body is the one PUT /settings takes and may be left out. A body that changes smtp_host, smtp_port, smtp_username or smtp_security, or turns smtp_skip_verify on, has to carry smtp_password while a password is stored, or it is refused with 400 under settings.smtp_password.required. A message the mail server did not take is answered with 502 and what went wrong in error_args.reason.
// @Tags         settings
// @Accept   json
// @Produce  json
// @Security  CSRFToken
// @Param   body  body  api.updateSettingsRequest  false  "Settings to test with instead of the stored ones"
// @Success  200  {object}  models.Response{data=api.alertTestSent}
// @Failure  400  {object}  api.errorBody  "A setting broke one of the rules, no mail server is set, or the body moves the mail target without a new password"
// @Failure  502  {object}  api.errorBody  "The mail server was not reached or did not take the message"
// @Router       /settings/alert/test-smtp [post]
func (h *SettingsHandler) TestAlertSMTP(c echo.Context) error {
	asked, refused := h.alertTestSettings(c, true)
	if refused != nil {
		return refused.answer(c)
	}

	if asked.SMTPHost == "" {
		return failure(c, http.StatusBadRequest, errAlertTestSMTPOff)
	}

	err := h.alerts.Mail(c.Request().Context(), alert.ConfigOf(asked).SMTP, testEvent())
	if err != nil {
		h.logger.Warn("a test alert was not delivered",
			logid.AlertTestFailed.Field(),
			zap.String("channel", "smtp"),
			zap.Error(err))

		return failure(c, http.StatusBadGateway, errAlertTestFailed, errorArgs{"reason": err.Error()})
	}

	h.logger.Info("sent a test alert", logid.AlertTestSent.Field(), zap.String("channel", "smtp"))

	return c.JSON(http.StatusOK, models.Response{
		Success: true,
		Data:    alertTestSent{Channel: "smtp"},
	})
}
