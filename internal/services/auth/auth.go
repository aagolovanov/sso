package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sso/internal/domain/models"
	"sso/internal/lib/jwt"
	"sso/internal/lib/logger/sl"
	"sso/internal/storage"
	"time"

	"crypto/rand"

	"golang.org/x/crypto/bcrypt"
)

type Auth struct {
	log             *slog.Logger
	accountSaver    AccountSaver
	accountProvider AccountProvider
	appProvider     AppProvider
	sessionSaver    SessionSaver
	sessionProvider SessionProvider
	tokenTTL        time.Duration
	refreshTokenTTL time.Duration
}

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
)

type AccountSaver interface {
	SaveAccount(ctx context.Context, email string, passHash []byte, role models.AccountRole, status models.AccountStatus, appId int64) (uid int64, err error)
	UpdatePassword(ctx context.Context, accountId int64, newPassHash []byte) (err error)
	UpdateStatus(ctx context.Context, accountId int64, status models.AccountStatus) (err error)
}

type AccountProvider interface {
	AccountByEmail(ctx context.Context, email string) (models.Account, error)
	AccountById(ctx context.Context, accountId int64) (models.Account, error)
	IsAdmin(ctx context.Context, accountId int64) (bool, error)
}

type AppProvider interface {
	App(ctx context.Context, appId int64) (models.App, error)
}

type SessionSaver interface {
	SaveSession(ctx context.Context, accountId int64, userAgent string, ipAddress string, token string, refreshToken string, expiresAt time.Time) (sessionID string, err error)
	RevokeSession(ctx context.Context, token string) (err error)
}

type SessionProvider interface {
	Sessions(ctx context.Context, accountId int64) ([]models.Session, error)
	Session(ctx context.Context, token string) (models.Session, error)
	SessionByRefreshToken(ctx context.Context, refreshToken string) (models.Session, error)
	RevokeSession(ctx context.Context, token string) (err error)
}

func New(
	log *slog.Logger,
	accountSaver AccountSaver,
	accountProvider AccountProvider,
	appProvider AppProvider,
	sessionSaver SessionSaver,
	sessionProvider SessionProvider,
	tokenTTL time.Duration,
	refreshTokenTTL time.Duration,
) *Auth {
	return &Auth{
		log:             log,
		accountSaver:    accountSaver,
		accountProvider: accountProvider,
		appProvider:     appProvider,
		sessionSaver:    sessionSaver,
		sessionProvider: sessionProvider,
		tokenTTL:        tokenTTL,
		refreshTokenTTL: refreshTokenTTL,
	}
}

// RegisterNewAccount registers a new account in the system, creates a session, and returns account ID.
func (a *Auth) RegisterNewAccount(ctx context.Context, email string, pass string, role models.AccountRole, appId int64) (int64, error) {
	const op = "Auth.RegisterNewAccount"

	log := a.log.With(
		slog.String("op", op),
		slog.String("email", email),
	)

	log.Info("registering account")

	passHash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		log.Error("failed to generate password hash", sl.Err(err))
		return 0, fmt.Errorf("%s: %w", op, err)
	}

	status := models.ACTIVE

	id, err := a.accountSaver.SaveAccount(ctx, email, passHash, role, status, appId)
	if err != nil {
		log.Error("failed to save account", sl.Err(err))
		return 0, fmt.Errorf("%s: %w", op, err)
	}

	return id, nil
}

// Login checks if account with given credentials exists in the system and returns access + refresh token.
//
// If account exists, but password is incorrect, returns error.
// If account doesn't exist, returns error.
func (a *Auth) Login(
	ctx context.Context,
	email string,
	password string,
	userAgent string,
	ipAddress string,
	appID int64,
) (string, string, error) {
	const op = "Auth.Login"

	log := a.log.With(
		slog.String("op", op),
		slog.String("username", email),
	)

	log.Info("attempting to login user")

	user, err := a.accountProvider.AccountByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, storage.ErrAccountNotFound) {
			a.log.Warn("user not found", sl.Err(err))
			return "", "", fmt.Errorf("%s: %w", op, ErrInvalidCredentials)
		}

		a.log.Error("failed to get user", sl.Err(err))
		return "", "", fmt.Errorf("%s: %w", op, err)
	}

	if err := bcrypt.CompareHashAndPassword(user.PassHash, []byte(password)); err != nil {
		a.log.Info("invalid credentials", sl.Err(err))
		return "", "", fmt.Errorf("%s: %w", op, ErrInvalidCredentials)
	}

	app, err := a.appProvider.App(ctx, appID)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", op, err)
	}

	log.Info("user logged in successfully")

	token, err := jwt.NewToken(user, app, a.tokenTTL)
	if err != nil {
		a.log.Error("failed to generate token", sl.Err(err))
		return "", "", fmt.Errorf("%s: %w", op, err)
	}

	refreshToken, err := generateRefreshToken()
	if err != nil {
		log.Error("failed to generate refresh token", sl.Err(err))
		return "", "", fmt.Errorf("%s: %w", op, err)
	}
	expiresAt := time.Now().Add(a.refreshTokenTTL)

	sessionID, err := a.sessionSaver.SaveSession(ctx, user.ID, userAgent, ipAddress, token, refreshToken, expiresAt)
	if err != nil {
		a.log.Error("failed to save session", sl.Err(err))
		return "", "", fmt.Errorf("%s: %w", op, err)
	}

	log.Info("session created", slog.String("session_id", sessionID))

	return token, refreshToken, nil
}

func generateRefreshToken() (string, error) {
	const tokenSize = 32
	token := make([]byte, tokenSize)

	if _, err := rand.Read(token); err != nil {
		return "", err
	}

	return base64.URLEncoding.EncodeToString(token), nil
}

// Logout logs out a user by terminating their sessions.
func (a *Auth) Logout(ctx context.Context, accountID int64) (bool, error) {
	const op = "Auth.Logout"

	log := a.log.With(
		slog.String("op", op),
		slog.Int64("account_id", accountID),
	)

	log.Info("logging out user")

	// Revoke all sessions for the given account ID.
	sessions, err := a.sessionProvider.Sessions(ctx, accountID)
	if err != nil {
		log.Error("failed to get sessions", sl.Err(err))
		return false, fmt.Errorf("%s: %w", op, err)
	}

	for _, session := range sessions {
		err := a.sessionSaver.RevokeSession(ctx, session.Token)
		if err != nil {
			log.Error("failed to revoke session", sl.Err(err))
			return false, fmt.Errorf("%s: %w", op, err)
		}
	}

	log.Info("user logged out successfully")
	return true, nil
}

func (a *Auth) ChangePassword(ctx context.Context, accountID int64, oldPassword, newPassword string) (bool, error) {
	const op = "Auth.ChangePassword"

	log := a.log.With(
		slog.String("op", op),
		slog.Int64("account_id", accountID),
	)

	log.Info("attempting to change password")

	account, err := a.accountProvider.AccountById(ctx, accountID)
	if err != nil {
		log.Error("failed to get account", sl.Err(err))
		return false, fmt.Errorf("%s: %w", op, err)
	}

	if err := bcrypt.CompareHashAndPassword(account.PassHash, []byte(oldPassword)); err != nil {
		log.Info("invalid old password", sl.Err(err))
		return false, fmt.Errorf("%s: %w", op, ErrInvalidCredentials)
	}

	newPassHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		log.Error("failed to hash new password", sl.Err(err))
		return false, fmt.Errorf("%s: %w", op, err)
	}

	err = a.accountSaver.UpdatePassword(ctx, accountID, newPassHash)
	if err != nil {
		log.Error("failed to update password", sl.Err(err))
		return false, fmt.Errorf("%s: %w", op, err)
	}

	log.Info("password changed successfully")
	return true, nil
}

// ChangeStatus changes the status of an account.
func (a *Auth) ChangeStatus(ctx context.Context, accountID int64, status models.AccountStatus) (models.AccountStatus, error) {
	const op = "Auth.ChangeStatus"

	log := a.log.With(
		slog.String("op", op),
		slog.Int64("account_id", accountID),
		slog.Int64("new_status", int64(status)),
	)

	log.Info("attempting to change account status")

	err := a.accountSaver.UpdateStatus(ctx, accountID, status)
	if err != nil {
		log.Error("failed to change status", sl.Err(err))
		return status, fmt.Errorf("%s: %w", op, err)
	}

	log.Info("status changed successfully")
	return status, nil
}

// GetActiveAccountSessions retrieves all active sessions for the given account ID.
func (a *Auth) GetActiveAccountSessions(ctx context.Context, accountID int64) ([]models.Session, error) {
	const op = "Auth.GetActiveAccountSessions"

	log := a.log.With(
		slog.String("op", op),
		slog.Int64("account_id", accountID),
	)

	log.Info("retrieving active sessions")

	sessions, err := a.sessionProvider.Sessions(ctx, accountID)
	if err != nil {
		log.Error("failed to retrieve sessions", sl.Err(err))
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	log.Info("sessions retrieved successfully")
	return sessions, nil
}

// RefreshAccountSession refreshes the account session by generating a new token and refresh token.
func (a *Auth) RefreshAccountSession(ctx context.Context, accountID int64, refreshToken string, userAgent string, ipAddress string) (string, string, int64, error) {
	const op = "Auth.RefreshAccountSession"

	log := a.log.With(
		slog.String("op", op),
		slog.Int64("account_id", accountID),
	)

	log.Info("attempting to get account")

	account, err := a.accountProvider.AccountById(ctx, accountID)
	if err != nil {
		log.Error("invalid account id", sl.Err(err))
		return "", "", 0, fmt.Errorf("%s: %w", op, err)
	}

	log.Info("attempting to get app")

	app, err := a.appProvider.App(ctx, account.AppId)
	if err != nil {
		log.Error("invalid app id", sl.Err(err))
		return "", "", 0, fmt.Errorf("%s: %w", op, err)
	}

	log.Info("attempting to refresh session")

	session, err := a.sessionProvider.SessionByRefreshToken(ctx, refreshToken)
	if err != nil {
		log.Error("invalid refresh token", sl.Err(err))
		return "", "", 0, fmt.Errorf("%s: %w", op, err)
	}

	if session.RefreshExpiresAt.Before(time.Now()) {
		log.Info("refresh token expired")
		return "", "", 0, fmt.Errorf("%s: %w", op, err)
	}

	newToken, err := jwt.NewToken(account, app, a.tokenTTL)
	if err != nil {
		log.Error("failed to generate new token", sl.Err(err))
		return "", "", 0, fmt.Errorf("%s: %w", op, err)
	}

	newRefreshToken, err := generateRefreshToken()
	if err != nil {
		log.Error("failed to generate new refresh token", sl.Err(err))
		return "", "", 0, fmt.Errorf("%s: %w", op, err)
	}

	expiresAt := time.Now().Add(a.refreshTokenTTL)

	sessionID, err := a.sessionSaver.SaveSession(ctx, accountID, userAgent, ipAddress, newToken, newRefreshToken, expiresAt)
	if err != nil {
		log.Error("failed to update session tokens", sl.Err(err))
		return "", "", 0, fmt.Errorf("%s: %w", op, err)
	}

	log.Info("session created", slog.String("session_id", sessionID))

	return newToken, newRefreshToken, expiresAt.Unix(), nil
}

// ValidateAccountSession validates if the token is still active.
func (a *Auth) ValidateAccountSession(ctx context.Context, token string) (bool, int64, error) {
	const op = "Auth.ValidateAccountSession"

	log := a.log.With(
		slog.String("op", op),
	)

	log.Info("validating session")

	session, err := a.sessionProvider.Session(ctx, token)
	if err != nil {
		log.Error("invalid token", sl.Err(err))
		return false, 0, fmt.Errorf("%s: %w", op, err)
	}

	if session.ExpiresAt.Before(time.Now()) {
		log.Info("session expired")
		return false, session.ExpiresAt.Unix(), nil
	}

	log.Info("session is valid")
	return true, session.ExpiresAt.Unix(), nil
}

// RevokeAccountSession revokes the session associated with the given token.
func (a *Auth) RevokeAccountSession(ctx context.Context, token string) (bool, error) {
	const op = "Auth.RevokeAccountSession"

	log := a.log.With(
		slog.String("op", op),
	)

	log.Info("revoking session")

	err := a.sessionProvider.RevokeSession(ctx, token)
	if err != nil {
		log.Error("failed to revoke session", sl.Err(err))
		return false, fmt.Errorf("%s: %w", op, err)
	}

	log.Info("session revoked successfully")
	return true, nil
}
