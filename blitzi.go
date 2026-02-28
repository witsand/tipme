package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BlitziClient is an HTTP client for the locally-running blitzi server.
type BlitziClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewBlitziClient(baseURL, token string) *BlitziClient {
	return &BlitziClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// BlitziInvoice is returned when creating a new Lightning invoice.
type BlitziInvoice struct {
	PaymentHash string `json:"payment_hash"`
	Invoice     string `json:"invoice"` // BOLT-11 string
}

// BlitziInvoiceStatus is returned when querying invoice status.
type BlitziInvoiceStatus struct {
	PaymentHash string `json:"payment_hash"`
	Paid        bool   `json:"paid"`
}

// CreateInvoice asks blitzi to create a new Lightning invoice.
func (c *BlitziClient) CreateInvoice(ctx context.Context, amountMsats int64, description string) (*BlitziInvoice, error) {
	body, _ := json.Marshal(map[string]any{
		"amount_msats": amountMsats,
		"description": description,
	})
	resp, err := c.do(ctx, http.MethodPost, "/invoice", body)
	if err != nil {
		return nil, err
	}
	var inv BlitziInvoice
	if err := json.Unmarshal(resp, &inv); err != nil {
		return nil, fmt.Errorf("decode invoice response: %w", err)
	}
	if inv.PaymentHash == "" || inv.Invoice == "" {
		return nil, fmt.Errorf("incomplete invoice response: %s", resp)
	}
	return &inv, nil
}

// WaitForPayment polls blitzi until the invoice identified by paymentHash is paid
// or the context is cancelled / deadline exceeded.
func (c *BlitziClient) WaitForPayment(ctx context.Context, paymentHash string) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			paid, err := c.checkInvoicePaid(ctx, paymentHash)
			if err != nil {
				// Log but keep trying; transient network errors are common.
				continue
			}
			if paid {
				return nil
			}
		}
	}
}

func (c *BlitziClient) checkInvoicePaid(ctx context.Context, paymentHash string) (bool, error) {
	resp, err := c.do(ctx, http.MethodGet, "/invoice/"+paymentHash, nil)
	if err != nil {
		return false, err
	}
	var status BlitziInvoiceStatus
	if err := json.Unmarshal(resp, &status); err != nil {
		return false, fmt.Errorf("decode status: %w", err)
	}
	return status.Paid, nil
}

// PayInvoice asks blitzi to pay a BOLT-11 invoice.
func (c *BlitziClient) PayInvoice(ctx context.Context, bolt11 string) error {
	body, _ := json.Marshal(map[string]any{
		"invoice": bolt11,
	})
	resp, err := c.do(ctx, http.MethodPost, "/pay", body)
	if err != nil {
		return err
	}
	var result struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		// Some blitzi builds return empty body on success; treat as success.
		return nil
	}
	if !result.Success && result.Error != "" {
		return fmt.Errorf("blitzi pay error: %s", result.Error)
	}
	return nil
}

// do executes an authenticated HTTP request to blitzi and returns the response body.
func (c *BlitziClient) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blitzi %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read blitzi response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("blitzi %s %s status %d: %s", method, path, resp.StatusCode, respBody)
	}
	return respBody, nil
}
