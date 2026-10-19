package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgconn"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"junglegaming/internal/adapters/auth"
	"junglegaming/internal/application"
	"junglegaming/internal/domain"
	"junglegaming/internal/identifier"
	"junglegaming/internal/platform/telemetry"
)

type API struct {
	service   *application.Service
	validator *auth.Validator
	metrics   *telemetry.Metrics
	logger    *slog.Logger
}

func NewAPI(service *application.Service, validator *auth.Validator, metrics *telemetry.Metrics, logger *slog.Logger) *API {
	return &API{service, validator, metrics, logger}
}

type contextKey int

const (
	principalKey contextKey = iota
	correlationKey
)

func NewRouter(health *Health, api *API) http.Handler {
	mux := http.NewServeMux()
	health.Register(mux)
	mux.Handle("POST /wallets", api.protect(true, http.HandlerFunc(api.openWallet)))
	mux.Handle("GET /wallets/{walletId}", api.protect(true, http.HandlerFunc(api.wallet)))
	mux.Handle("GET /wallets/{walletId}/ledger", api.protect(true, http.HandlerFunc(api.ledger)))
	mux.Handle("POST /wallets/{walletId}/reconciliation", api.protect(true, http.HandlerFunc(api.reconcile)))
	mux.Handle("POST /wagering/transactions", api.protect(false, http.HandlerFunc(api.process)))
	mux.Handle("GET /wagering/transactions/{transactionId}", api.protect(false, http.HandlerFunc(api.transaction)))
	mux.Handle("GET /providers/{providerId}/wagering/transactions/{externalTransactionId}", api.protect(false, http.HandlerFunc(api.externalTransaction)))
	mux.Handle("GET /metrics", api.protect(true, api.metrics.Handler()))
	return mux
}

func (a *API) protect(internal bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlation, err := identifier.New()
		if err != nil {
			writeJSON(w, 500, map[string]string{"code": "INTERNAL_ERROR"})
			return
		}
		w.Header().Set("X-Correlation-Id", correlation)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			a.unauthorized(w)
			return
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 16384 {
			a.unauthorized(w)
			return
		}
		principal, err := a.validator.Verify(ctx, parts[1])
		if err != nil {
			a.unauthorized(w)
			return
		}
		if principal.Internal != internal {
			writeJSON(w, 403, map[string]string{"code": "FORBIDDEN"})
			return
		}
		ctx = context.WithValue(ctx, principalKey, principal)
		ctx = context.WithValue(ctx, correlationKey, correlation)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func (a *API) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="wagercore"`)
	writeJSON(w, 401, map[string]string{"code": "UNAUTHORIZED"})
}
func trace(r *http.Request) application.Trace {
	correlation, _ := r.Context().Value(correlationKey).(string)
	return application.Trace{CorrelationID: correlation}
}
func principal(r *http.Request) auth.Principal {
	value, _ := r.Context().Value(principalKey).(auth.Principal)
	return value
}

func decode(w http.ResponseWriter, r *http.Request, target any) error {
	if content := r.Header.Get("Content-Type"); !strings.EqualFold(strings.TrimSpace(strings.Split(content, ";")[0]), "application/json") {
		return errors.New("Content-Type must be application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid request JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("body must contain one JSON object")
	}
	return nil
}

type walletResponse struct {
	ID       string       `json:"id"`
	PlayerID string       `json:"playerId"`
	Balance  domain.Money `json:"balance"`
	Version  int64        `json:"version"`
}

func walletBody(wallet *domain.Wallet) walletResponse {
	state := wallet.Snapshot()
	return walletResponse{state.ID, state.PlayerID, state.Balance, state.Version}
}

func (a *API) openWallet(w http.ResponseWriter, r *http.Request) {
	var request struct {
		PlayerID       string       `json:"playerId"`
		InitialBalance domain.Money `json:"initialBalance"`
	}
	if err := decode(w, r, &request); err != nil {
		badRequest(w, err)
		return
	}
	wallet, err := a.service.OpenWallet(r.Context(), request.PlayerID, request.InitialBalance, trace(r))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	a.logger.Info("wallet_opened", "walletId", wallet.Snapshot().ID, "correlationId", trace(r).CorrelationID)
	writeJSON(w, 201, walletBody(wallet))
}
func (a *API) wallet(w http.ResponseWriter, r *http.Request) {
	if !validID(r.PathValue("walletId")) {
		badRequest(w, domain.ErrInvalidID)
		return
	}
	wallet, err := a.service.Reader.Wallet(r.Context(), r.PathValue("walletId"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	writeJSON(w, 200, walletBody(wallet))
}
func (a *API) process(w http.ResponseWriter, r *http.Request) {
	var request application.WagerRequest
	if err := decode(w, r, &request); err != nil {
		badRequest(w, err)
		return
	}
	if request.ProviderID != principal(r).ProviderID {
		writeJSON(w, 403, map[string]string{"code": "FORBIDDEN"})
		return
	}
	keys := r.Header.Values("Idempotency-Key")
	if len(keys) != 1 || keys[0] == "" {
		badRequest(w, errors.New("Idempotency-Key is required once"))
		return
	}
	result, err := a.service.Process(r.Context(), request, keys[0], trace(r), nil)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	a.logger.Info("wager_processed", "transactionId", result.TransactionID, "walletId", request.WalletID, "providerId", request.ProviderID, "correlationId", trace(r).CorrelationID, "status", result.Status, "idempotentReplay", result.IdempotentReplay)
	status := 200
	if result.Status == domain.Rejected {
		status = 422
	}
	if result.Status == domain.Pending || result.Status == domain.PendingReference {
		status = 202
	}
	writeJSON(w, status, result)
}
func (a *API) transaction(w http.ResponseWriter, r *http.Request) {
	if !validID(r.PathValue("transactionId")) {
		badRequest(w, domain.ErrInvalidID)
		return
	}
	transaction, err := a.service.Reader.Transaction(r.Context(), r.PathValue("transactionId"))
	a.respondTransaction(w, r, transaction, err)
}
func (a *API) externalTransaction(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("providerId") != principal(r).ProviderID {
		writeJSON(w, 404, map[string]string{"code": "NOT_FOUND"})
		return
	}
	transaction, err := a.service.Reader.ExternalTransaction(r.Context(), principal(r).ProviderID, r.PathValue("externalTransactionId"))
	a.respondTransaction(w, r, transaction, err)
}
func (a *API) respondTransaction(w http.ResponseWriter, r *http.Request, transaction *domain.WagerTransaction, err error) {
	if err != nil {
		a.failure(w, r, err)
		return
	}
	s := transaction.Snapshot()
	if s.Input.ProviderID != principal(r).ProviderID {
		writeJSON(w, 404, map[string]string{"code": "NOT_FOUND"})
		return
	}
	writeJSON(w, 200, struct {
		TransactionID string `json:"transactionId"`
		application.WagerRequest
		Status      domain.TransactionStatus `json:"status"`
		FailureCode domain.FailureCode       `json:"failureCode,omitempty"`
		Balance     *domain.Money            `json:"balance,omitempty"`
		CreatedAt   time.Time                `json:"createdAt"`
		UpdatedAt   time.Time                `json:"updatedAt"`
	}{s.Input.ID, application.WagerRequest{ProviderID: s.Input.ProviderID, ExternalTransactionID: s.Input.ExternalTransactionID, PlayerID: s.Input.PlayerID, WalletID: s.Input.WalletID, RoundID: s.Input.RoundID, GameID: s.Input.GameID, Kind: s.Input.Kind, Money: s.Input.Money, ReferenceExternalTransactionID: s.Input.ReferenceExternalTransactionID}, s.Status, s.FailureCode, s.ResultBalance, s.CreatedAt, s.UpdatedAt})
}
func (a *API) ledger(w http.ResponseWriter, r *http.Request) {
	if !validID(r.PathValue("walletId")) {
		badRequest(w, domain.ErrInvalidID)
		return
	}
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		number, err := strconv.Atoi(value)
		if err != nil || number < 1 || number > 100 {
			badRequest(w, errors.New("limit must be between 1 and 100"))
			return
		}
		limit = number
	}
	page, err := a.service.Reader.Ledger(r.Context(), r.PathValue("walletId"), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		a.failure(w, r, err)
		return
	}
	type entry struct {
		ID            string           `json:"id"`
		WalletID      string           `json:"walletId"`
		TransactionID string           `json:"transactionId"`
		Direction     domain.Direction `json:"direction"`
		Money         domain.Money     `json:"money"`
		BalanceBefore domain.Money     `json:"balanceBefore"`
		BalanceAfter  domain.Money     `json:"balanceAfter"`
		WalletVersion int64            `json:"walletVersion"`
		CreatedAt     time.Time        `json:"createdAt"`
	}
	entries := make([]entry, 0, len(page.Entries))
	for _, e := range page.Entries {
		entries = append(entries, entry{e.ID, e.WalletID, e.TransactionID, e.Direction, e.Money, e.BalanceBefore, e.BalanceAfter, e.WalletVersion, e.CreatedAt})
	}
	writeJSON(w, 200, struct {
		Entries    []entry `json:"entries"`
		NextCursor string  `json:"nextCursor,omitempty"`
	}{entries, page.NextCursor})
}
func (a *API) reconcile(w http.ResponseWriter, r *http.Request) {
	if !validID(r.PathValue("walletId")) {
		badRequest(w, domain.ErrInvalidID)
		return
	}
	result, err := a.service.Reader.Reconcile(r.Context(), r.PathValue("walletId"))
	if err != nil {
		a.failure(w, r, err)
		return
	}
	a.metrics.Reconciled(result.Consistent)
	a.logger.Info("wallet_reconciled", "walletId", result.WalletID, "correlationId", trace(r).CorrelationID, "consistent", result.Consistent, "checkedEntries", result.CheckedEntries)
	writeJSON(w, 200, result)
}
func badRequest(w http.ResponseWriter, err error) {
	writeJSON(w, 400, map[string]string{"code": "INVALID_REQUEST", "message": err.Error()})
}
func (a *API) failure(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrNotFound):
		writeJSON(w, 404, map[string]string{"code": "NOT_FOUND"})
	case errors.Is(err, application.ErrConflict):
		writeJSON(w, 409, map[string]string{"code": "CONFLICT"})
	case errors.Is(err, application.ErrUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		w.Header().Set("Retry-After", "1")
		var pgError *pgconn.PgError
		code, constraint := "", ""
		if errors.As(err, &pgError) {
			code, constraint = pgError.Code, pgError.ConstraintName
		}
		a.logger.Warn("request_unavailable", "correlationId", trace(r).CorrelationID, "sqlState", code, "constraint", constraint)
		writeJSON(w, 503, map[string]string{"code": "TEMPORARILY_UNAVAILABLE"})
	case errors.Is(err, application.ErrInvalidCursor), errors.Is(err, application.ErrWalletMismatch), errors.Is(err, domain.ErrInvalidID), errors.Is(err, domain.ErrInvalidAmount), errors.Is(err, domain.ErrInvalidCurrency), errors.Is(err, domain.ErrInvalidTransaction), errors.Is(err, domain.ErrInvalidReference):
		badRequest(w, err)
	default:
		a.logger.Error("request_failed", "correlationId", trace(r).CorrelationID)
		writeJSON(w, 500, map[string]string{"code": "INTERNAL_ERROR"})
	}
}
func validID(value string) bool {
	return domain.ValidateID(value) == nil
}
