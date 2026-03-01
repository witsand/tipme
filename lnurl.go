package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ── Bech32 LNURL encoding ────────────────────────────────────────────────────

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var bech32Generator = []uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func bech32Polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= bech32Generator[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	result := make([]byte, len(hrp)*2+1)
	for i, c := range hrp {
		result[i] = byte(c) >> 5
		result[i+len(hrp)+1] = byte(c) & 31
	}
	result[len(hrp)] = 0
	return result
}

func bech32CreateChecksum(hrp string, data []byte) []byte {
	values := append(bech32HRPExpand(hrp), data...)
	values = append(values, []byte{0, 0, 0, 0, 0, 0}...)
	pm := bech32Polymod(values) ^ 1
	checksum := make([]byte, 6)
	for i := range checksum {
		checksum[i] = byte((pm >> uint(5*(5-i))) & 31)
	}
	return checksum
}

func convertBits(data []byte, fromBits, toBits uint, pad bool) ([]byte, error) {
	acc := 0
	bits := uint(0)
	var result []byte
	maxv := (1 << toBits) - 1
	for _, value := range data {
		acc = (acc << fromBits) | int(value)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			result = append(result, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			result = append(result, byte((acc<<(toBits-bits))&maxv))
		}
	} else if bits >= fromBits || ((acc<<(toBits-bits))&maxv) != 0 {
		return nil, fmt.Errorf("invalid padding in bit conversion")
	}
	return result, nil
}

// EncodeLNURL bech32-encodes a URL as an LNURL string (uppercase).
func EncodeLNURL(rawURL string) (string, error) {
	converted, err := convertBits([]byte(rawURL), 8, 5, true)
	if err != nil {
		return "", err
	}
	checksum := bech32CreateChecksum("lnurl", converted)
	combined := append(converted, checksum...)

	var sb strings.Builder
	sb.WriteString("lnurl1")
	for _, c := range combined {
		if int(c) >= len(bech32Charset) {
			return "", fmt.Errorf("invalid data value %d", c)
		}
		sb.WriteByte(bech32Charset[c])
	}
	return strings.ToUpper(sb.String()), nil
}

// ── Lightning Address resolution ─────────────────────────────────────────────

// LNURLPayParams holds the metadata from a Lightning address LNURL-Pay endpoint.
type LNURLPayParams struct {
	Callback    string `json:"callback"`
	MinSendable int64  `json:"minSendable"` // millisatoshis
	MaxSendable int64  `json:"maxSendable"` // millisatoshis
}

var lnHTTP = &http.Client{Timeout: 15 * time.Second}

// ResolveLightningAddress resolves user@domain to LNURL-Pay params.
func ResolveLightningAddress(address string) (*LNURLPayParams, error) {
	parts := strings.SplitN(address, "@", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid lightning address: %s", address)
	}
	user, domain := parts[0], parts[1]
	url := fmt.Sprintf("https://%s/.well-known/lnurlp/%s", domain, user)

	resp, err := lnHTTP.Get(url)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", address, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("resolve %s: status %d", address, resp.StatusCode)
	}

	var params LNURLPayParams
	if err := json.Unmarshal(body, &params); err != nil {
		return nil, fmt.Errorf("decode lnurlp params: %w", err)
	}
	if params.Callback == "" {
		return nil, fmt.Errorf("empty callback from %s", address)
	}
	return &params, nil
}

// GetInvoiceFromCallback calls a LNURL-Pay callback to get a BOLT-11 invoice.
func GetInvoiceFromCallback(callbackURL string, amountMsats int64) (string, error) {
	sep := "?"
	if strings.Contains(callbackURL, "?") {
		sep = "&"
	}
	url := fmt.Sprintf("%s%samount=%d", callbackURL, sep, amountMsats)

	resp, err := lnHTTP.Get(url)
	if err != nil {
		return "", fmt.Errorf("callback request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("callback status %d: %s", resp.StatusCode, body)
	}

	var result struct {
		PR     string `json:"pr"`
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode callback response: %w", err)
	}
	if result.Status == "ERROR" {
		return "", fmt.Errorf("lnurl callback error: %s", result.Reason)
	}
	if result.PR == "" {
		return "", fmt.Errorf("empty invoice from callback")
	}
	return result.PR, nil
}

// RefundToLightningAddress sends amountMsats to a Lightning address via LNURL-Pay.
func RefundToLightningAddress(address string, amountMsats int64) error {
	params, err := ResolveLightningAddress(address)
	if err != nil {
		return fmt.Errorf("resolve address: %w", err)
	}

	// Skip dust that falls below the target's minimum.
	if amountMsats < params.MinSendable {
		return fmt.Errorf("refund amount %d msats is below target minSendable %d msats (dust)", amountMsats, params.MinSendable)
	}
	if amountMsats > params.MaxSendable {
		amountMsats = params.MaxSendable // cap to max
	}

	// Round down to whole sats — some wallets reject sub-sat msats amounts.
	amountMsats = (amountMsats / 1000) * 1000

	invoice, err := GetInvoiceFromCallback(params.Callback, amountMsats)
	if err != nil {
		return fmt.Errorf("get invoice from callback: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return blitziClient.PayInvoice(ctx, invoice)
}
