package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/petoshi/qday-walletd/internal/daemon"
)

const maxBody = 16 << 10

type API struct {
	service *daemon.Service
	token   string
}

func New(service *daemon.Service, token string) http.Handler {
	a := &API{service: service, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("GET /v1/status", a.auth(a.status))
	mux.HandleFunc("POST /v1/wallet/unlock", a.auth(a.unlock))
	mux.HandleFunc("POST /v1/wallet/lock", a.auth(a.lock))
	mux.HandleFunc("GET /v1/addresses", a.auth(a.addresses))
	mux.HandleFunc("POST /v1/addresses", a.auth(a.createAddress))
	mux.HandleFunc("GET /v1/balance", a.auth(a.balance))
	mux.HandleFunc("GET /v1/deposits", a.auth(a.deposits))
	mux.HandleFunc("GET /v1/withdrawals", a.auth(a.withdrawals))
	mux.HandleFunc("POST /v1/withdrawals", a.auth(a.createWithdrawal))
	mux.HandleFunc("GET /v1/withdrawals/{requestID}", a.auth(a.withdrawal))
	mux.HandleFunc("POST /v1/swap-keys", a.auth(a.createSwapKeys))
	mux.HandleFunc("GET /v1/swaps", a.auth(a.swaps))
	mux.HandleFunc("POST /v1/swaps", a.auth(a.registerSwap))
	mux.HandleFunc("GET /v1/swaps/{swapID}", a.auth(a.swap))
	mux.HandleFunc("POST /v1/swaps/{swapID}/fund", a.auth(a.fundSwap))
	mux.HandleFunc("POST /v1/swaps/{swapID}/claim", a.auth(a.claimSwap))
	mux.HandleFunc("POST /v1/swaps/{swapID}/refund", a.auth(a.refundSwap))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *API) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(provided) != len(a.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	} else if err := d.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body must contain one JSON value")
		return false
	}
	return true
}

func pagination(r *http.Request) (limit, offset int, err error) {
	limit, offset = 50, 0
	if value := r.URL.Query().Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil {
			return
		}
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		offset, err = strconv.Atoi(value)
	}
	if err == nil && (limit < 1 || limit > 200 || offset < 0) {
		err = errors.New("limit must be 1..200 and offset must be nonnegative")
	}
	return
}

func (a *API) ready(w http.ResponseWriter, _ *http.Request) {
	status, err := a.service.Status()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if synced, _ := status["synced"].(bool); !synced {
		writeJSON(w, http.StatusServiceUnavailable, status)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *API) status(w http.ResponseWriter, _ *http.Request) {
	status, err := a.service.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *API) unlock(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &request) {
		return
	}
	started := time.Now()
	err := a.service.Unlock(request.Password)
	request.Password = ""
	// Argon2 normally dominates this endpoint. Keep malformed requests from
	// becoming a cheap online password oracle if the keystore implementation
	// changes later.
	if remaining := 500*time.Millisecond - time.Since(started); remaining > 0 {
		time.Sleep(remaining)
	}
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"unlocked": true})
}

func (a *API) lock(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if !decode(w, r, &request) {
		return
	}
	a.service.Lock()
	writeJSON(w, http.StatusOK, map[string]bool{"unlocked": false})
}

func (a *API) addresses(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	addresses, total, err := a.service.Addresses(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"addresses": addresses, "total": total, "limit": limit, "offset": offset})
}

func (a *API) createAddress(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Reference string `json:"reference"`
	}
	if !decode(w, r, &request) {
		return
	}
	address, err := a.service.NewDepositAddress(request.Reference)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, address)
}

func (a *API) balance(w http.ResponseWriter, _ *http.Request) {
	balance, err := a.service.Balance()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, balance)
}

func (a *API) deposits(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	deposits, next, err := a.service.Deposits(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deposits": deposits, "nextOffset": next})
}

func (a *API) createWithdrawal(w http.ResponseWriter, r *http.Request) {
	var request daemon.WithdrawalRequest
	if !decode(w, r, &request) {
		return
	}
	withdrawal, created, err := a.service.CreateWithdrawal(r.Context(), request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, withdrawal)
}

func (a *API) withdrawal(w http.ResponseWriter, r *http.Request) {
	withdrawal, ok, err := a.service.Withdrawal(r.PathValue("requestID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "withdrawal not found")
		return
	}
	writeJSON(w, http.StatusOK, withdrawal)
}

func (a *API) withdrawals(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	withdrawals, err := a.service.Withdrawals(limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"withdrawals": withdrawals, "limit": limit, "offset": offset})
}

func (a *API) createSwapKeys(w http.ResponseWriter, r *http.Request) {
	var request daemon.SwapKeyRequest
	if !decode(w, r, &request) {
		return
	}
	keys, created, err := a.service.CreateSwapKeys(request.SwapID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, keys)
}

func (a *API) registerSwap(w http.ResponseWriter, r *http.Request) {
	var request daemon.RegisterSwapRequest
	if !decode(w, r, &request) {
		return
	}
	swap, created, err := a.service.RegisterSwap(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, swap)
}

func (a *API) swaps(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := pagination(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	swaps, err := a.service.Swaps(limit, offset)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"swaps": swaps, "limit": limit, "offset": offset})
}

func (a *API) swap(w http.ResponseWriter, r *http.Request) {
	swap, ok, err := a.service.Swap(r.PathValue("swapID"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	} else if !ok {
		writeError(w, http.StatusNotFound, "atomic swap not found")
		return
	}
	writeJSON(w, http.StatusOK, swap)
}

func (a *API) fundSwap(w http.ResponseWriter, r *http.Request) {
	var request daemon.FundSwapRequest
	if !decode(w, r, &request) {
		return
	}
	action, created, err := a.service.FundSwap(r.Context(), r.PathValue("swapID"), request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, action)
}

func (a *API) claimSwap(w http.ResponseWriter, r *http.Request) {
	var request daemon.SpendSwapRequest
	if !decode(w, r, &request) {
		return
	}
	action, created, err := a.service.ClaimSwap(r.Context(), r.PathValue("swapID"), request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, action)
}

func (a *API) refundSwap(w http.ResponseWriter, r *http.Request) {
	var request daemon.SpendSwapRequest
	if !decode(w, r, &request) {
		return
	}
	action, created, err := a.service.RefundSwap(r.Context(), r.PathValue("swapID"), request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, action)
}
