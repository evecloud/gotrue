package api

import (
	"errors"
	"net/http"

	"github.com/evecloud/auth/internal/api/sms_provider"
	"github.com/evecloud/auth/internal/conf"
	"github.com/evecloud/auth/internal/crypto"
	"github.com/evecloud/auth/internal/models"
	"github.com/evecloud/auth/internal/storage"
)

const InvalidNonceMessage = "Nonce has expired or is invalid"

// Reauthenticate sends a reauthentication otp to either the user's email or phone
func (a *API) Reauthenticate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	db := a.db.WithContext(ctx)

	user := getUser(ctx)
	email, phone := user.GetEmail(), user.GetPhone()

	if email == "" && phone == "" {
		return badRequestError(ErrorCodeValidationFailed, "Reauthentication requires the user to have an email or a phone number")
	}

	if email != "" {
		if !user.IsConfirmed() {
			return unprocessableEntityError(ErrorCodeEmailNotConfirmed, "Please verify your email first.")
		}
	} else if phone != "" {
		if !user.IsPhoneConfirmed() {
			return unprocessableEntityError(ErrorCodePhoneNotConfirmed, "Please verify your phone first.")
		}
	}

	messageID := ""
	err := db.Transaction(func(tx *storage.Connection) error {
		if terr := models.NewAuditLogEntry(r, tx, user, models.UserReauthenticateAction, "", nil); terr != nil {
			return terr
		}
		if email != "" {
			return a.sendReauthenticationOtp(r, tx, user)
		} else if phone != "" {
			mID, err := a.sendPhoneConfirmation(r, tx, user, phone, phoneReauthenticationOtp, sms_provider.SMSProvider)
			if err != nil {
				return err
			}

			messageID = mID
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, MaxFrequencyLimitError) {
			reason := ErrorCodeOverEmailSendRateLimit
			if phone != "" {
				reason = ErrorCodeOverSMSSendRateLimit
			}

			return tooManyRequestsError(reason, "For security purposes, you can only request this once every 60 seconds")
		}
		return err
	}

	ret := map[string]any{}
	if messageID != "" {
		ret["message_id"] = messageID

	}

	return sendJSON(w, http.StatusOK, ret)
}

// verifyReauthentication checks if the nonce provided is valid
func (a *API) verifyReauthentication(nonce string, tx *storage.Connection, config *conf.GlobalConfiguration, user *models.User) error {
	if user.ReauthenticationToken == "" || user.ReauthenticationSentAt == nil {
		return unprocessableEntityError(ErrorCodeReauthenticationNotValid, InvalidNonceMessage)
	}
	var isValid bool
	if user.GetEmail() != "" {
		tokenHash := crypto.GenerateTokenHash(user.GetEmail(), nonce)
		isValid = isOtpValid(tokenHash, user.ReauthenticationToken, user.ReauthenticationSentAt, config.Mailer.OtpExp)
	} else if user.GetPhone() != "" {
		if config.Sms.IsTwilioVerifyProvider() {
			smsProvider, _ := sms_provider.GetSmsProvider(*config)
			if err := smsProvider.(*sms_provider.TwilioVerifyProvider).VerifyOTP(string(user.Phone), nonce); err != nil {
				return forbiddenError(ErrorCodeOTPExpired, "Token has expired or is invalid").WithInternalError(err)
			}
			return nil
		} else {
			tokenHash := crypto.GenerateTokenHash(user.GetPhone(), nonce)
			isValid = isOtpValid(tokenHash, user.ReauthenticationToken, user.ReauthenticationSentAt, config.Sms.OtpExp)
		}
	} else {
		return unprocessableEntityError(ErrorCodeReauthenticationNotValid, "Reauthentication requires an email or a phone number")
	}
	if !isValid {
		return unprocessableEntityError(ErrorCodeReauthenticationNotValid, InvalidNonceMessage)
	}
	if err := user.ConfirmReauthentication(tx); err != nil {
		return internalServerError("Error during reauthentication").WithInternalError(err)
	}
	return nil
}
