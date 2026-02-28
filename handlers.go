package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
)

var lightningAddressRE = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// writeJSON serialises v as JSON and writes it to w.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// lnurlError returns an LNURL-spec error response.
func lnurlError(w http.ResponseWriter, reason string) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ERROR",
		"reason": reason,
	})
}

// ── POST /api/vouchers/invoice ───────────────────────────────────────────────

type createInvoiceRequest struct {
	LightningAddress string `json:"lightning_address"`
	Count            int    `json:"count"`
	ExpirySeconds    int64  `json:"expiry_seconds"`
}

func handleCreateInvoice(w http.ResponseWriter, r *http.Request) {
	var req createInvoiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	// Validate.
	if !lightningAddressRE.MatchString(req.LightningAddress) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid lightning_address"})
		return
	}
	if req.Count < 1 || req.Count > cfg.MaxVouchersPerRequest {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("count must be between 1 and %d", cfg.MaxVouchersPerRequest),
		})
		return
	}
	if req.ExpirySeconds <= 0 {
		req.ExpirySeconds = cfg.DefaultRelativeExpirySecs
	}

	feeSats := cfg.FeePerVoucherSats * int64(req.Count)
	feeMsats := feeSats * 1000

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	inv, err := blitziClient.CreateInvoice(ctx, feeMsats, fmt.Sprintf("TipMe: create %d voucher(s)", req.Count))
	if err != nil {
		log.Printf("blitzi CreateInvoice: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "failed to create payment invoice"})
		return
	}

	if err := database.InsertCreationRequest(inv.PaymentHash, req.LightningAddress, req.Count, req.ExpirySeconds, feeMsats); err != nil {
		log.Printf("InsertCreationRequest: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}

	// Background goroutine: wait for payment then create vouchers.
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), time.Hour)
		defer bgCancel()

		if err := blitziClient.WaitForPayment(bgCtx, inv.PaymentHash); err != nil {
			// Timed out or context cancelled.
			if dbErr := database.UpdateCreationRequestStatus(inv.PaymentHash, "expired"); dbErr != nil {
				log.Printf("UpdateCreationRequestStatus expired: %v", dbErr)
			}
			return
		}

		// Payment confirmed — create vouchers.
		payIDs := make([]string, req.Count)
		withdrawIDs := make([]string, req.Count)
		for i := 0; i < req.Count; i++ {
			payIDs[i] = uuid.New().String()
			withdrawIDs[i] = uuid.New().String()
		}

		if err := database.InsertVouchers(payIDs, withdrawIDs, inv.PaymentHash, req.LightningAddress, req.ExpirySeconds); err != nil {
			log.Printf("InsertVouchers: %v", err)
			if dbErr := database.UpdateCreationRequestStatus(inv.PaymentHash, "expired"); dbErr != nil {
				log.Printf("UpdateCreationRequestStatus: %v", dbErr)
			}
			return
		}

		if err := database.UpdateCreationRequestStatus(inv.PaymentHash, "complete"); err != nil {
			log.Printf("UpdateCreationRequestStatus complete: %v", err)
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"invoice":      inv.Invoice,
		"payment_hash": inv.PaymentHash,
		"fee_sats":     feeSats,
	})
}

// ── GET /api/vouchers/status/:payment_hash ───────────────────────────────────

func handleVoucherStatus(w http.ResponseWriter, r *http.Request) {
	paymentHash := r.PathValue("payment_hash")

	creq, err := database.GetCreationRequest(paymentHash)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	if creq.Status != "complete" {
		writeJSON(w, http.StatusOK, map[string]string{"status": creq.Status})
		return
	}

	vouchers, err := database.GetVouchersByCreationHash(paymentHash)
	if err != nil {
		log.Printf("GetVouchersByCreationHash: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database error"})
		return
	}

	type voucherResp struct {
		LNURLPay          string `json:"lnurl_pay"`
		LNURLWithdraw     string `json:"lnurl_withdraw"`
		LightningAddress  string `json:"lightning_address"`
		AbsoluteExpiry    string `json:"absolute_expiry"`
		RelativeExpirySec int64  `json:"relative_expiry_seconds"`
	}

	resp := make([]voucherResp, 0, len(vouchers))
	for _, v := range vouchers {
		payURL := fmt.Sprintf("%s/pay/%s", cfg.BaseURL, v.PayID)
		withURL := fmt.Sprintf("%s/withdraw/%s", cfg.BaseURL, v.WithdrawID)

		lnurlPay, err := EncodeLNURL(payURL)
		if err != nil {
			log.Printf("EncodeLNURL pay: %v", err)
			continue
		}
		lnurlWith, err := EncodeLNURL(withURL)
		if err != nil {
			log.Printf("EncodeLNURL withdraw: %v", err)
			continue
		}

		absExpiry := v.CreatedAt.Add(time.Duration(cfg.VoucherAbsoluteExpirySecs) * time.Second)
		resp = append(resp, voucherResp{
			LNURLPay:          lnurlPay,
			LNURLWithdraw:     lnurlWith,
			LightningAddress:  v.LightningAddress,
			AbsoluteExpiry:    absExpiry.UTC().Format(time.RFC3339),
			RelativeExpirySec: v.ExpirySeconds,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "complete",
		"vouchers": resp,
	})
}

// ── GET /pay/:pay_id (LNURL-Pay step 1) ─────────────────────────────────────

func handleLNURLPay(w http.ResponseWriter, r *http.Request) {
	payID := r.PathValue("pay_id")

	v, err := database.GetVoucherByPayID(payID)
	if err != nil {
		lnurlError(w, "voucher not found")
		return
	}
	if !v.IsActive() {
		lnurlError(w, "voucher is not active")
		return
	}

	callbackURL := fmt.Sprintf("%s/pay/%s/callback", cfg.BaseURL, payID)
	writeJSON(w, http.StatusOK, map[string]any{
		"tag":         "payRequest",
		"callback":    callbackURL,
		"minSendable": 100000,   // 100 sats in msats
		"maxSendable": 1000000000, // 1,000,000 sats in msats
		"metadata":    `[["text/plain","Tip via TipMe"]]`,
	})
}

// ── GET /pay/:pay_id/callback (LNURL-Pay step 2) ────────────────────────────

func handleLNURLPayCallback(w http.ResponseWriter, r *http.Request) {
	payID := r.PathValue("pay_id")
	amountStr := r.URL.Query().Get("amount")
	if amountStr == "" {
		lnurlError(w, "missing amount parameter")
		return
	}

	var amountMsats int64
	if _, err := fmt.Sscan(amountStr, &amountMsats); err != nil || amountMsats <= 0 {
		lnurlError(w, "invalid amount")
		return
	}

	// Re-check active conditions.
	v, err := database.GetVoucherByPayID(payID)
	if err != nil {
		lnurlError(w, "voucher not found")
		return
	}
	if !v.IsActive() {
		lnurlError(w, "voucher is not active")
		return
	}

	// Calculate fee.
	feeMsats := int64(math.Max(
		float64(cfg.FundingFeeMinMsats),
		math.Floor(float64(amountMsats)*cfg.FundingFeePercent),
	))
	creditedMsats := amountMsats - feeMsats
	if creditedMsats <= 0 {
		lnurlError(w, "amount too small to cover fee")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Create blitzi invoice for the full amount (server collects fee by not forwarding it).
	inv, err := blitziClient.CreateInvoice(ctx, amountMsats, "TipMe voucher funding")
	if err != nil {
		log.Printf("blitzi CreateInvoice (pay callback): %v", err)
		lnurlError(w, "failed to create invoice")
		return
	}

	invoiceID := uuid.New().String()
	if err := database.InsertPayInvoice(invoiceID, payID, inv.PaymentHash, amountMsats, creditedMsats); err != nil {
		log.Printf("InsertPayInvoice: %v", err)
		lnurlError(w, "database error")
		return
	}

	// Background goroutine: wait for payment then credit or refund.
	go func() {
		bgCtx, bgCancel := context.WithTimeout(context.Background(), time.Hour)
		defer bgCancel()

		if err := blitziClient.WaitForPayment(bgCtx, inv.PaymentHash); err != nil {
			log.Printf("WaitForPayment (pay_id=%s): %v", payID, err)
			return
		}

		// Use a transaction to re-check active state and credit atomically.
		tx, err := database.Begin()
		if err != nil {
			log.Printf("Begin tx (pay_id=%s): %v", payID, err)
			return
		}
		defer tx.Rollback()

		voucher, err := getVoucherByPayIDTx(tx, payID)
		if err != nil {
			log.Printf("getVoucherByPayIDTx: %v", err)
			return
		}

		if voucher.IsActive() {
			// Still active — credit it.
			if err := CreditVoucherTx(tx, payID, creditedMsats, inv.PaymentHash); err != nil {
				log.Printf("CreditVoucherTx: %v", err)
				return
			}
			if err := tx.Commit(); err != nil {
				log.Printf("Commit credit: %v", err)
			}
		} else {
			// Voucher became inactive while invoice was open — refund the payer.
			tx.Rollback()
			log.Printf("voucher %s became inactive after payment; attempting refund of %d msats to %s",
				payID, creditedMsats, voucher.LightningAddress)
			if err := RefundToLightningAddress(voucher.LightningAddress, creditedMsats); err != nil {
				log.Printf("refund failed for pay_id=%s: %v", payID, err)
			}
		}
	}()

	writeJSON(w, http.StatusOK, map[string]any{
		"pr":     inv.Invoice,
		"routes": []any{},
	})
}

// ── GET /withdraw/:withdraw_id (LNURL-Withdraw step 1) ──────────────────────

func handleLNURLWithdraw(w http.ResponseWriter, r *http.Request) {
	withdrawID := r.PathValue("withdraw_id")

	v, err := database.GetVoucherByWithdrawID(withdrawID)
	if err != nil {
		lnurlError(w, "voucher not found")
		return
	}
	if !v.IsActive() {
		lnurlError(w, "voucher is not active")
		return
	}
	if v.TotalPaidMsats <= 0 {
		lnurlError(w, "voucher has no balance")
		return
	}

	// Generate a random k1.
	k1Bytes := make([]byte, 32)
	if _, err := rand.Read(k1Bytes); err != nil {
		lnurlError(w, "internal error")
		return
	}
	k1 := hex.EncodeToString(k1Bytes)

	if err := database.InsertWithdrawSession(k1, withdrawID); err != nil {
		log.Printf("InsertWithdrawSession: %v", err)
		lnurlError(w, "database error")
		return
	}

	callbackURL := fmt.Sprintf("%s/withdraw/%s/callback", cfg.BaseURL, withdrawID)
	balance := v.TotalPaidMsats

	writeJSON(w, http.StatusOK, map[string]any{
		"tag":                "withdrawRequest",
		"callback":           callbackURL,
		"k1":                 k1,
		"defaultDescription": "TipMe withdrawal",
		"minWithdrawable":    balance,
		"maxWithdrawable":    balance,
	})
}

// ── GET /withdraw/:withdraw_id/callback (LNURL-Withdraw step 2) ─────────────

func handleLNURLWithdrawCallback(w http.ResponseWriter, r *http.Request) {
	withdrawID := r.PathValue("withdraw_id")
	k1 := r.URL.Query().Get("k1")
	pr := r.URL.Query().Get("pr")

	if k1 == "" || pr == "" {
		lnurlError(w, "missing k1 or pr parameter")
		return
	}

	// Validate k1 and mark used atomically; get the payID.
	payID, err := database.ValidateAndUseWithdrawSession(k1, withdrawID)
	if err != nil {
		lnurlError(w, "invalid or already-used k1: "+err.Error())
		return
	}

	// Re-check voucher active + balance.
	v, err := database.GetVoucherByWithdrawID(withdrawID)
	if err != nil {
		lnurlError(w, "voucher not found")
		return
	}
	if !v.IsActive() {
		lnurlError(w, "voucher is not active")
		return
	}
	if v.TotalPaidMsats <= 0 {
		lnurlError(w, "voucher has no balance")
		return
	}

	// Pay the invoice via blitzi.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	if err := blitziClient.PayInvoice(ctx, pr); err != nil {
		log.Printf("PayInvoice withdraw (withdraw_id=%s): %v", withdrawID, err)
		lnurlError(w, "payment failed")
		return
	}

	// Payment succeeded — deactivate the voucher.
	tx, err := database.Begin()
	if err != nil {
		log.Printf("Begin tx (deactivate withdraw_id=%s): %v", withdrawID, err)
		// Payment was sent but we couldn't deactivate — log and return OK to wallet.
		log.Printf("CRITICAL: voucher %s paid out but not deactivated", payID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "OK"})
		return
	}
	defer tx.Rollback()

	if err := DeactivateVoucherTx(tx, payID); err != nil {
		log.Printf("DeactivateVoucherTx: %v", err)
		tx.Rollback()
		log.Printf("CRITICAL: voucher %s paid out but not deactivated", payID)
		writeJSON(w, http.StatusOK, map[string]string{"status": "OK"})
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("CRITICAL: Commit deactivate for payID=%s failed: %v", payID, err)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "OK"})
}

// ── Automated Refund Job ─────────────────────────────────────────────────────

func runRefundJob(ctx context.Context) error {
	expired, err := database.GetExpiredVouchersForRefund()
	if err != nil {
		return fmt.Errorf("GetExpiredVouchersForRefund: %w", err)
	}

	log.Printf("refund job: found %d expired voucher(s) with balance", len(expired))

	for _, v := range expired {
		func(v *Voucher) {
			log.Printf("refund job: refunding %d msats to %s (pay_id=%s)",
				v.TotalPaidMsats, v.LightningAddress, v.PayID)

			if err := RefundToLightningAddress(v.LightningAddress, v.TotalPaidMsats); err != nil {
				log.Printf("refund job: failed to refund pay_id=%s: %v", v.PayID, err)
				// Don't deactivate if refund failed.
				return
			}

			if err := database.DeactivateVoucher(v.PayID); err != nil {
				log.Printf("refund job: DeactivateVoucher pay_id=%s: %v", v.PayID, err)
				return
			}
			log.Printf("refund job: successfully refunded and deactivated pay_id=%s", v.PayID)
		}(v)
	}
	return nil
}
