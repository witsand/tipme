package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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
		PayInfoURL        string `json:"pay_info_url"`
		WithdrawInfoURL   string `json:"withdraw_info_url"`
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
			LNURLPay:         lnurlPay,
			LNURLWithdraw:    lnurlWith,
			PayInfoURL:       fmt.Sprintf("%s/pay/info?lightning=%s", cfg.BaseURL, lnurlPay),
			WithdrawInfoURL:  fmt.Sprintf("%s/withdraw/info?lightning=%s", cfg.BaseURL, lnurlWith),
			LightningAddress: v.LightningAddress,
			AbsoluteExpiry:   absExpiry.UTC().Format(time.RFC3339),
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

	payURL := fmt.Sprintf("%s/pay/%s", cfg.BaseURL, payID)
	lnurlEncoded, _ := EncodeLNURL(payURL)
	callbackURL := fmt.Sprintf("%s/pay/%s/callback", cfg.BaseURL, payID)
	infoURL := fmt.Sprintf("%s/pay/info?lightning=%s", cfg.BaseURL, lnurlEncoded)
	writeJSON(w, http.StatusOK, map[string]any{
		"tag":         "payRequest",
		"callback":    callbackURL,
		"minSendable": cfg.MinVoucherPayAmountSats * 1000,
		"maxSendable": cfg.MaxVoucherPayAmountSats * 1000,
		"metadata":    `[["text/plain","Tip via TipMe"]]`,
		"url":         infoURL,
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
	feeMsats := int64(float64(cfg.FundingFeeMinMsats) + math.Floor(float64(amountMsats)*cfg.FundingFeePercent))
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

	withURL := fmt.Sprintf("%s/withdraw/%s", cfg.BaseURL, withdrawID)
	lnurlEncoded, _ := EncodeLNURL(withURL)
	callbackURL := fmt.Sprintf("%s/withdraw/%s/callback", cfg.BaseURL, withdrawID)
	infoURL := fmt.Sprintf("%s/withdraw/info?lightning=%s", cfg.BaseURL, lnurlEncoded)
	balance := v.TotalPaidMsats

	writeJSON(w, http.StatusOK, map[string]any{
		"tag":                "withdrawRequest",
		"callback":           callbackURL,
		"k1":                 k1,
		"defaultDescription": "TipMe withdrawal",
		"minWithdrawable":    balance,
		"maxWithdrawable":    balance,
		"url":                infoURL,
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
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			// Unknown outcome — deactivate to prevent potential double-pay.
			log.Printf("CRITICAL: withdraw payment outcome unknown for withdraw_id=%s (%d msats), deactivating to prevent double-pay: %v",
				withdrawID, v.TotalPaidMsats, err)
			if deactErr := database.DeactivateVoucher(payID); deactErr != nil {
				log.Printf("CRITICAL: failed to deactivate withdraw_id=%s after timeout: %v", withdrawID, deactErr)
			}
			lnurlError(w, "payment timed out")
		} else {
			// Definitive failure — voucher stays active so the user can retry.
			log.Printf("PayInvoice withdraw (withdraw_id=%s): %v", withdrawID, err)
			lnurlError(w, "payment failed")
		}
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

			// Deactivate before paying to prevent double-payment if the
			// payment succeeds but the HTTP response times out.
			if err := database.DeactivateVoucher(v.PayID); err != nil {
				log.Printf("refund job: DeactivateVoucher pay_id=%s: %v", v.PayID, err)
				return
			}

			if err := RefundToLightningAddress(v.LightningAddress, v.TotalPaidMsats); err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					// Unknown outcome — keep deactivated, flag for manual recovery.
					log.Printf("CRITICAL: refund payment outcome unknown for pay_id=%s (%d msats owed to %s): %v",
						v.PayID, v.TotalPaidMsats, v.LightningAddress, err)
				} else {
					// Definitive failure — restore voucher so the job retries next run.
					log.Printf("refund job: payment failed for pay_id=%s, re-activating: %v", v.PayID, err)
					if reErr := database.ReactivateVoucher(v.PayID, v.TotalPaidMsats); reErr != nil {
						log.Printf("CRITICAL: refund job: failed to re-activate pay_id=%s (%d msats owed to %s): %v",
							v.PayID, v.TotalPaidMsats, v.LightningAddress, reErr)
					}
				}
				return
			}

			log.Printf("refund job: successfully refunded and deactivated pay_id=%s", v.PayID)
		}(v)
	}
	return nil
}

// ── Pay Info Page ────────────────────────────────────────────────────────────

func handlePayInfo(w http.ResponseWriter, r *http.Request) {
	lightning := r.URL.Query().Get("lightning")
	if lightning == "" {
		http.Error(w, "missing lightning parameter", http.StatusBadRequest)
		return
	}
	rawURL, err := DecodeLNURL(lightning)
	if err != nil {
		http.Error(w, "invalid LNURL: "+err.Error(), http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		http.Error(w, "invalid URL in LNURL", http.StatusBadRequest)
		return
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) < 2 || segments[0] != "pay" {
		http.Error(w, "not a pay LNURL", http.StatusBadRequest)
		return
	}
	payID := segments[1]

	v, err := database.GetVoucherByPayID(payID)
	if err != nil {
		http.Error(w, "voucher not found", http.StatusNotFound)
		return
	}
	invoices, err := database.GetPaidInvoicesByPayID(payID)
	if err != nil {
		log.Printf("GetPaidInvoicesByPayID: %v", err)
	}

	isActive := v.IsActive()
	balanceSats := v.TotalPaidMsats / 1000

	badgeClass, badgeText := "badge-active", "Active"
	if !isActive {
		badgeClass, badgeText = "badge-inactive", "Expired / Claimed"
	}

	var timeRemainingHTML string
	if isActive {
		now := time.Now()
		absExpiry := v.CreatedAt.Add(time.Duration(cfg.VoucherAbsoluteExpirySecs) * time.Second)
		effectiveExpiry := absExpiry
		if v.LastFundedAt != nil {
			relExpiry := v.LastFundedAt.Add(time.Duration(v.ExpirySeconds) * time.Second)
			if relExpiry.Before(effectiveExpiry) {
				effectiveExpiry = relExpiry
			}
		}
		remaining := effectiveExpiry.Sub(now)
		days := int(remaining.Hours()) / 24
		hours := int(remaining.Hours()) % 24
		var remainStr string
		if days > 0 {
			remainStr = fmt.Sprintf("%dd %dh", days, hours)
		} else {
			remainStr = fmt.Sprintf("%dh", hours)
		}
		timeRemainingHTML = fmt.Sprintf(`<div><div class="stat-label">Expires in</div><div class="stat-value">%s</div></div>`, remainStr)
	}

	var txHTML string
	if len(invoices) > 0 {
		var rows strings.Builder
		for _, inv := range invoices {
			rows.WriteString(fmt.Sprintf("<tr><td>%s</td><td>%d sats</td></tr>",
				inv.PaidAt.UTC().Format("2 Jan 2006 15:04 UTC"),
				inv.CreditedMsats/1000,
			))
		}
		txHTML = fmt.Sprintf(`<hr><div class="section-label">Funding History</div><table><thead><tr><th>Date</th><th>Amount</th></tr></thead><tbody>%s</tbody></table>`, rows.String())
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, payInfoHTML, badgeClass, badgeText, balanceSats, timeRemainingHTML, txHTML, lightning)
}

// ── Withdraw Info Page ───────────────────────────────────────────────────────

func handleWithdrawInfo(w http.ResponseWriter, r *http.Request) {
	lightning := r.URL.Query().Get("lightning")
	if lightning == "" {
		http.Error(w, "missing lightning parameter", http.StatusBadRequest)
		return
	}
	rawURL, err := DecodeLNURL(lightning)
	if err != nil {
		http.Error(w, "invalid LNURL: "+err.Error(), http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		http.Error(w, "invalid URL in LNURL", http.StatusBadRequest)
		return
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(segments) < 2 || segments[0] != "withdraw" {
		http.Error(w, "not a withdraw LNURL", http.StatusBadRequest)
		return
	}
	withdrawID := segments[1]

	v, err := database.GetVoucherByWithdrawID(withdrawID)
	if err != nil {
		http.Error(w, "voucher not found", http.StatusNotFound)
		return
	}

	balanceSats := v.TotalPaidMsats / 1000
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, withdrawInfoHTML, balanceSats, lightning)
}

// ── HTML templates ───────────────────────────────────────────────────────────

const payInfoHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TipMe Voucher</title>
<script src="https://cdn.jsdelivr.net/npm/qrcodejs@1.0.0/qrcode.min.js"></script>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,-apple-system,sans-serif;background:#f5f5f5;color:#111;padding:1rem}
.card{max-width:440px;margin:1rem auto;background:#fff;border-radius:14px;padding:1.5rem;box-shadow:0 2px 16px rgba(0,0,0,.09)}
h1{font-size:1.25rem;margin-bottom:1rem}
.badge{display:inline-block;padding:3px 12px;border-radius:20px;font-size:.82rem;font-weight:600}
.badge-active{background:#dcfce7;color:#16a34a}
.badge-inactive{background:#fee2e2;color:#dc2626}
.stats{display:grid;grid-template-columns:1fr 1fr;gap:1rem;margin:1.25rem 0}
.stat-label{font-size:.75rem;color:#666;font-weight:600;text-transform:uppercase;letter-spacing:.06em;margin-bottom:2px}
.stat-value{font-size:1.5rem;font-weight:700}
.orange{color:#f7931a}
hr{border:none;border-top:1px solid #eee;margin:1.25rem 0}
.section-label{font-size:.75rem;color:#666;font-weight:600;text-transform:uppercase;letter-spacing:.06em;margin-bottom:.5rem}
table{width:100%%;border-collapse:collapse;font-size:.88rem;margin-top:.5rem}
th{text-align:left;color:#888;font-weight:600;padding:.35rem 0;border-bottom:1px solid #eee}
td{padding:.4rem 0;border-bottom:1px solid #f5f5f5}
.qr-wrap{display:flex;flex-direction:column;align-items:center;gap:.5rem;margin-top:1.25rem}
.qr-hint{font-size:.82rem;color:#666;text-align:center}
</style>
</head>
<body>
<div class="card">
<h1>⚡ TipMe Voucher</h1>
<span class="badge %s">%s</span>
<div class="stats">
<div><div class="stat-label">Balance</div><div class="stat-value orange">%d sats</div></div>
%s
</div>
%s
<hr>
<div class="qr-wrap">
<div id="qr"></div>
<p class="qr-hint">Scan with a Lightning wallet to fund this voucher</p>
</div>
</div>
<script>
new QRCode(document.getElementById('qr'),{text:%q,width:200,height:200,correctLevel:QRCode.CorrectLevel.M});
</script>
</body>
</html>`

const withdrawInfoHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Claim Your Sats</title>
<script src="https://cdn.jsdelivr.net/npm/qrcodejs@1.0.0/qrcode.min.js"></script>
<style>
*,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
body{font-family:system-ui,-apple-system,sans-serif;background:#f5f5f5;color:#111;padding:1rem}
.card{max-width:440px;margin:1rem auto;background:#fff;border-radius:14px;padding:1.5rem;box-shadow:0 2px 16px rgba(0,0,0,.09)}
h1{font-size:1.25rem;margin-bottom:.25rem}
.balance{font-size:2.5rem;font-weight:800;color:#f7931a;margin:.75rem 0 1.25rem}
.step{display:flex;gap:1rem;margin-bottom:1.1rem;align-items:flex-start}
.step-num{background:#f7931a;color:#fff;border-radius:50%%;width:28px;height:28px;display:flex;align-items:center;justify-content:center;font-weight:700;font-size:.9rem;flex-shrink:0;margin-top:2px}
.step-text{line-height:1.5;font-size:.95rem}
.step-text strong{display:block;font-size:1rem;margin-bottom:2px}
a{color:#f7931a;text-decoration:none;font-weight:600}
hr{border:none;border-top:1px solid #eee;margin:1.25rem 0}
.qr-wrap{display:flex;flex-direction:column;align-items:center;gap:.5rem}
.qr-hint{font-size:.82rem;color:#666;text-align:center}
.note{font-size:.82rem;color:#666;background:#fef9ec;border:1px solid #fde68a;padding:.75rem;border-radius:8px;margin-top:1.25rem}
</style>
</head>
<body>
<div class="card">
<h1>💸 Claim Your Sats</h1>
<p style="color:#666;font-size:.9rem">This voucher is worth:</p>
<div class="balance">%d sats</div>
<div class="step">
<div class="step-num">1</div>
<div class="step-text"><strong>Download Blink Wallet</strong>Get the free <a href="https://blink.sv" target="_blank">Blink</a> app from <a href="https://blink.sv" target="_blank">blink.sv</a> — available on the App Store and Google Play.</div>
</div>
<div class="step">
<div class="step-num">2</div>
<div class="step-text"><strong>Create a free account</strong>Sign up with just your phone number. No ID or bank account needed.</div>
</div>
<div class="step">
<div class="step-num">3</div>
<div class="step-text"><strong>Scan the QR code below</strong>In Blink, tap <strong>Receive</strong> then use the in-app scanner to scan this QR code. Your sats will arrive instantly.</div>
</div>
<hr>
<div class="qr-wrap">
<div id="qr"></div>
<p class="qr-hint">Scan with Blink or any Lightning wallet to claim your sats</p>
</div>
<div class="note">⚠️ This voucher can only be claimed once. Have your wallet ready before scanning.</div>
</div>
<script>
new QRCode(document.getElementById('qr'),{text:%q,width:200,height:200,correctLevel:QRCode.CorrectLevel.M});
</script>
</body>
</html>`
