package main

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// DB wraps sql.DB with our application queries.
type DB struct {
	*sql.DB
}

// VoucherCreationRequest represents a pending voucher batch creation.
type VoucherCreationRequest struct {
	PaymentHash      string
	LightningAddress string
	Count            int
	ExpirySeconds    int64
	FeeMsats         int64
	Status           string
	CreatedAt        time.Time
}

// Voucher represents a single tipme voucher.
type Voucher struct {
	PayID            string
	WithdrawID       string
	CreationHash     string
	LightningAddress string
	TotalPaidMsats   int64
	LastFundedAt     *time.Time
	ExpirySeconds    int64
	Active           bool
	CreatedAt        time.Time
}

// IsActive checks all three active conditions.
func (v *Voucher) IsActive() bool {
	if !v.Active {
		return false
	}
	now := time.Now()
	// 1-year absolute expiry from creation.
	if now.After(v.CreatedAt.Add(time.Duration(cfg.VoucherAbsoluteExpirySecs) * time.Second)) {
		return false
	}
	// Relative funding expiry.
	if v.LastFundedAt != nil {
		if now.After(v.LastFundedAt.Add(time.Duration(v.ExpirySeconds) * time.Second)) {
			return false
		}
	}
	return true
}

func initDB(path string) (*DB, error) {
	sqlDB, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// SQLite handles one writer at a time; serialise writes in the app layer.
	sqlDB.SetMaxOpenConns(1)

	db := &DB{sqlDB}
	if err := db.createSchema(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	return db, nil
}

func (db *DB) createSchema() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS voucher_creation_requests (
			payment_hash      TEXT PRIMARY KEY,
			lightning_address TEXT NOT NULL,
			count             INTEGER NOT NULL,
			expiry_seconds    INTEGER NOT NULL,
			fee_msats         INTEGER NOT NULL,
			status            TEXT NOT NULL DEFAULT 'pending',
			created_at        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS vouchers (
			pay_id                TEXT PRIMARY KEY,
			withdraw_id           TEXT NOT NULL UNIQUE,
			creation_request_hash TEXT REFERENCES voucher_creation_requests(payment_hash),
			lightning_address     TEXT NOT NULL,
			total_paid_msats      INTEGER NOT NULL DEFAULT 0,
			last_funded_at        DATETIME,
			expiry_seconds        INTEGER NOT NULL,
			active                INTEGER NOT NULL DEFAULT 1,
			created_at            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_vouchers_withdraw_id ON vouchers(withdraw_id)`,
		`CREATE INDEX IF NOT EXISTS idx_vouchers_creation_hash ON vouchers(creation_request_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_vouchers_refund ON vouchers(active, total_paid_msats, last_funded_at, created_at)`,
		`CREATE TABLE IF NOT EXISTS pay_invoices (
			id             TEXT PRIMARY KEY,
			pay_id         TEXT NOT NULL REFERENCES vouchers(pay_id),
			payment_hash   TEXT NOT NULL UNIQUE,
			amount_msats   INTEGER NOT NULL,
			credited_msats INTEGER NOT NULL,
			paid           INTEGER NOT NULL DEFAULT 0,
			created_at     DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			paid_at        DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS withdraw_sessions (
			k1          TEXT PRIMARY KEY,
			withdraw_id TEXT NOT NULL REFERENCES vouchers(withdraw_id),
			created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			used        INTEGER NOT NULL DEFAULT 0,
			used_at     DATETIME
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			n := len(s); if n > 40 { n = 40 }
			return fmt.Errorf("exec %q: %w", s[:n], err)
		}
	}
	return nil
}

// ── Voucher Creation Requests ────────────────────────────────────────────────

func (db *DB) InsertCreationRequest(paymentHash, address string, count int, expirySecs, feeMsats int64) error {
	_, err := db.Exec(
		`INSERT INTO voucher_creation_requests (payment_hash, lightning_address, count, expiry_seconds, fee_msats)
		 VALUES (?, ?, ?, ?, ?)`,
		paymentHash, address, count, expirySecs, feeMsats,
	)
	return err
}

func (db *DB) UpdateCreationRequestStatus(paymentHash, status string) error {
	_, err := db.Exec(
		`UPDATE voucher_creation_requests SET status=? WHERE payment_hash=?`,
		status, paymentHash,
	)
	return err
}

func (db *DB) GetCreationRequest(paymentHash string) (*VoucherCreationRequest, error) {
	row := db.QueryRow(
		`SELECT payment_hash, lightning_address, count, expiry_seconds, fee_msats, status, created_at
		 FROM voucher_creation_requests WHERE payment_hash=?`,
		paymentHash,
	)
	var req VoucherCreationRequest
	if err := row.Scan(
		&req.PaymentHash, &req.LightningAddress, &req.Count,
		&req.ExpirySeconds, &req.FeeMsats, &req.Status, &req.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &req, nil
}

// ── Vouchers ─────────────────────────────────────────────────────────────────

func (db *DB) InsertVouchers(payIDs, withdrawIDs []string, creationHash, address string, expirySecs int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO vouchers (pay_id, withdraw_id, creation_request_hash, lightning_address, expiry_seconds)
		 VALUES (?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i := range payIDs {
		if _, err := stmt.Exec(payIDs[i], withdrawIDs[i], creationHash, address, expirySecs); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) GetVouchersByCreationHash(hash string) ([]*Voucher, error) {
	rows, err := db.Query(
		`SELECT pay_id, withdraw_id, creation_request_hash, lightning_address,
		        total_paid_msats, last_funded_at, expiry_seconds, active, created_at
		 FROM vouchers WHERE creation_request_hash=? ORDER BY rowid`,
		hash,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVouchers(rows)
}

func (db *DB) GetVoucherByPayID(payID string) (*Voucher, error) {
	row := db.QueryRow(
		`SELECT pay_id, withdraw_id, creation_request_hash, lightning_address,
		        total_paid_msats, last_funded_at, expiry_seconds, active, created_at
		 FROM vouchers WHERE pay_id=?`,
		payID,
	)
	return scanVoucher(row)
}

func (db *DB) GetVoucherByWithdrawID(withdrawID string) (*Voucher, error) {
	row := db.QueryRow(
		`SELECT pay_id, withdraw_id, creation_request_hash, lightning_address,
		        total_paid_msats, last_funded_at, expiry_seconds, active, created_at
		 FROM vouchers WHERE withdraw_id=?`,
		withdrawID,
	)
	return scanVoucher(row)
}

// getVoucherByPayIDTx reads a voucher inside an existing transaction.
func getVoucherByPayIDTx(tx *sql.Tx, payID string) (*Voucher, error) {
	row := tx.QueryRow(
		`SELECT pay_id, withdraw_id, creation_request_hash, lightning_address,
		        total_paid_msats, last_funded_at, expiry_seconds, active, created_at
		 FROM vouchers WHERE pay_id=?`,
		payID,
	)
	return scanVoucher(row)
}

func scanVoucher(row *sql.Row) (*Voucher, error) {
	var v Voucher
	var lastFunded sql.NullTime
	var activeInt int
	var creationHash sql.NullString
	if err := row.Scan(
		&v.PayID, &v.WithdrawID, &creationHash, &v.LightningAddress,
		&v.TotalPaidMsats, &lastFunded, &v.ExpirySeconds, &activeInt, &v.CreatedAt,
	); err != nil {
		return nil, err
	}
	v.Active = activeInt == 1
	if lastFunded.Valid {
		v.LastFundedAt = &lastFunded.Time
	}
	if creationHash.Valid {
		v.CreationHash = creationHash.String
	}
	return &v, nil
}

func scanVouchers(rows *sql.Rows) ([]*Voucher, error) {
	var vouchers []*Voucher
	for rows.Next() {
		var v Voucher
		var lastFunded sql.NullTime
		var activeInt int
		var creationHash sql.NullString
		if err := rows.Scan(
			&v.PayID, &v.WithdrawID, &creationHash, &v.LightningAddress,
			&v.TotalPaidMsats, &lastFunded, &v.ExpirySeconds, &activeInt, &v.CreatedAt,
		); err != nil {
			return nil, err
		}
		v.Active = activeInt == 1
		if lastFunded.Valid {
			v.LastFundedAt = &lastFunded.Time
		}
		if creationHash.Valid {
			v.CreationHash = creationHash.String
		}
		vouchers = append(vouchers, &v)
	}
	return vouchers, rows.Err()
}

// CreditVoucherTx credits a voucher and marks its invoice paid inside a transaction.
func CreditVoucherTx(tx *sql.Tx, payID string, creditedMsats int64, paymentHash string) error {
	if _, err := tx.Exec(
		`UPDATE vouchers SET total_paid_msats=total_paid_msats+?, last_funded_at=CURRENT_TIMESTAMP
		 WHERE pay_id=?`,
		creditedMsats, payID,
	); err != nil {
		return err
	}
	_, err := tx.Exec(
		`UPDATE pay_invoices SET paid=1, paid_at=CURRENT_TIMESTAMP WHERE payment_hash=?`,
		paymentHash,
	)
	return err
}

// DeactivateVoucherTx zeroes balance and marks active=0 inside a transaction.
func DeactivateVoucherTx(tx *sql.Tx, payID string) error {
	_, err := tx.Exec(
		`UPDATE vouchers SET total_paid_msats=0, active=0 WHERE pay_id=?`,
		payID,
	)
	return err
}

// DeactivateVoucher zeroes balance and marks active=0 (standalone, for refund job).
func (db *DB) DeactivateVoucher(payID string) error {
	_, err := db.Exec(
		`UPDATE vouchers SET total_paid_msats=0, active=0 WHERE pay_id=?`,
		payID,
	)
	return err
}

// ReactivateVoucher restores a voucher's balance and marks it active again.
// Used when a refund payment definitively fails so the job retries next run.
func (db *DB) ReactivateVoucher(payID string, balanceMsats int64) error {
	_, err := db.Exec(
		`UPDATE vouchers SET total_paid_msats=?, active=1 WHERE pay_id=?`,
		balanceMsats, payID,
	)
	return err
}

// GetExpiredVouchersForRefund returns active vouchers that have balance and have crossed
// either their relative or absolute expiry boundary.
func (db *DB) GetExpiredVouchersForRefund() ([]*Voucher, error) {
	rows, err := db.Query(
		`SELECT pay_id, withdraw_id, creation_request_hash, lightning_address,
		        total_paid_msats, last_funded_at, expiry_seconds, active, created_at
		 FROM vouchers
		 WHERE active=1 AND total_paid_msats>0
		 AND (
		   (last_funded_at IS NOT NULL
		    AND (CAST(strftime('%s','now') AS INTEGER) - CAST(strftime('%s',last_funded_at) AS INTEGER)) >= expiry_seconds)
		   OR
		   ((CAST(strftime('%s','now') AS INTEGER) - CAST(strftime('%s',created_at) AS INTEGER)) >= ?)
		 )`,
		cfg.VoucherAbsoluteExpirySecs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanVouchers(rows)
}

// ── Pay Invoices ──────────────────────────────────────────────────────────────

func (db *DB) InsertPayInvoice(id, payID, paymentHash string, amountMsats, creditedMsats int64) error {
	_, err := db.Exec(
		`INSERT INTO pay_invoices (id, pay_id, payment_hash, amount_msats, credited_msats)
		 VALUES (?, ?, ?, ?, ?)`,
		id, payID, paymentHash, amountMsats, creditedMsats,
	)
	return err
}

// ── Withdraw Sessions ─────────────────────────────────────────────────────────

func (db *DB) InsertWithdrawSession(k1, withdrawID string) error {
	_, err := db.Exec(
		`INSERT INTO withdraw_sessions (k1, withdraw_id) VALUES (?, ?)`,
		k1, withdrawID,
	)
	return err
}

// ValidateAndUseWithdrawSession checks k1 is unused and belongs to withdrawID,
// marks it used, and returns the associated payID — all in one transaction.
func (db *DB) ValidateAndUseWithdrawSession(k1, withdrawID string) (payID string, err error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Verify k1 belongs to this withdraw_id and is unused.
	var used int
	var storedWithdrawID string
	if err := tx.QueryRow(
		`SELECT withdraw_id, used FROM withdraw_sessions WHERE k1=?`, k1,
	).Scan(&storedWithdrawID, &used); err != nil {
		return "", fmt.Errorf("k1 not found")
	}
	if storedWithdrawID != withdrawID {
		return "", fmt.Errorf("k1 mismatch")
	}
	if used != 0 {
		return "", fmt.Errorf("k1 already used")
	}

	// Mark k1 used.
	if _, err := tx.Exec(
		`UPDATE withdraw_sessions SET used=1, used_at=CURRENT_TIMESTAMP WHERE k1=?`, k1,
	); err != nil {
		return "", err
	}

	// Get the pay_id for the voucher.
	if err := tx.QueryRow(
		`SELECT pay_id FROM vouchers WHERE withdraw_id=?`, withdrawID,
	).Scan(&payID); err != nil {
		return "", fmt.Errorf("voucher not found for withdraw_id")
	}

	return payID, tx.Commit()
}
