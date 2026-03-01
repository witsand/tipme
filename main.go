package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Config holds all server configuration loaded from environment variables.
type Config struct {
	BaseURL                   string
	BlitziURL                 string
	BlitziToken               string
	DBPath                    string
	Port                      string
	FeePerVoucherSats         int64
	FundingFeeMinMsats        int64
	FundingFeePercent         float64
	MaxVouchersPerRequest     int
	VoucherAbsoluteExpirySecs int64
	DefaultRelativeExpirySecs int64
	MinVoucherPayAmountSats   int64
	MaxVoucherPayAmountSats   int64
}

var cfg Config
var database *DB
var blitziClient *BlitziClient

func loadConfig() {
	cfg.BaseURL = envStr("BASE_URL", "http://localhost:8080")
	cfg.BlitziURL = envStr("BLITZI_URL", "http://localhost:3000")
	cfg.BlitziToken = envStr("BLITZI_TOKEN", "lmPZA5z6BNGewZykHhnETd7TQearB5so")
	cfg.DBPath = envStr("DB_PATH", "./tipme.db")
	cfg.Port = envStr("PORT", "8080")
	cfg.FeePerVoucherSats = envInt64("FEE_PER_VOUCHER_SATS", 10)
	cfg.FundingFeeMinMsats = envInt64("FUNDING_FEE_MIN_MSATS", 2000)
	cfg.FundingFeePercent = envFloat64("FUNDING_FEE_PERCENT", 0.004)
	cfg.MaxVouchersPerRequest = int(envInt64("MAX_VOUCHERS_PER_REQUEST", 10))
	cfg.VoucherAbsoluteExpirySecs = envInt64("VOUCHER_ABSOLUTE_EXPIRY_SECS", 31536000)
	cfg.DefaultRelativeExpirySecs = envInt64("DEFAULT_RELATIVE_EXPIRY_SECS", 2592000)
	cfg.MinVoucherPayAmountSats = envInt64("MIN_VOUCHER_PAY_AMOUNT_SATS", 100)
	cfg.MaxVoucherPayAmountSats = envInt64("MAX_VOUCHER_PAY_AMOUNT_SATS", 200000)
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			log.Fatalf("invalid %s: %v", key, err)
		}
		return n
	}
	return def
}

func envFloat64(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			log.Fatalf("invalid %s: %v", key, err)
		}
		return f
	}
	return def
}

func main() {
	loadConfig()

	var err error
	database, err = initDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("failed to init db: %v", err)
	}
	defer database.Close()

	blitziClient = NewBlitziClient(cfg.BlitziURL, cfg.BlitziToken)

	// Run refund job at startup and then daily.
	go runRefundJobLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", serveIndex)
	mux.HandleFunc("POST /api/vouchers/invoice", handleCreateInvoice)
	mux.HandleFunc("GET /api/vouchers/status/{payment_hash}", handleVoucherStatus)
	mux.HandleFunc("GET /pay/info", handlePayInfo)
	mux.HandleFunc("GET /pay/{pay_id}/callback", handleLNURLPayCallback)
	mux.HandleFunc("GET /pay/{pay_id}", handleLNURLPay)
	mux.HandleFunc("GET /withdraw/info", handleWithdrawInfo)
	mux.HandleFunc("GET /withdraw/{withdraw_id}/callback", handleLNURLWithdrawCallback)
	mux.HandleFunc("GET /withdraw/{withdraw_id}", handleLNURLWithdraw)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		mux.ServeHTTP(w, r)
	})

	log.Printf("TipMe listening on :%s  base=%s", cfg.Port, cfg.BaseURL)
	if err := http.ListenAndServe(":"+cfg.Port, handler); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func runRefundJobLoop() {
	ctx := context.Background()
	if err := runRefundJob(ctx); err != nil {
		log.Printf("startup refund job error: %v", err)
	}
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		if err := runRefundJob(ctx); err != nil {
			log.Printf("daily refund job error: %v", err)
		}
	}
}
