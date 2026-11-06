package database

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"gorm.io/gorm"
)

// boundedPool measures and limits acquisition for every GORM operation. pgx
// still caches protocol statements; GORM's connection-bound statement cache is
// disabled so executions cannot bypass this boundary.
type boundedPool struct {
	pool   *sql.DB
	budget time.Duration
}

var _ gorm.ConnPoolBeginner = (*boundedPool)(nil)
var _ gorm.GetDBConnector = (*boundedPool)(nil)

func (p *boundedPool) GetDBConn() (*sql.DB, error) { return p.pool, nil }
func (p *boundedPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	ctx, cancel := context.WithTimeout(ctx, p.budget)
	defer cancel()
	return p.pool.PrepareContext(ctx, query)
}
func (p *boundedPool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	conn, err := Acquire(ctx, p.pool, p.budget)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return conn.ExecContext(ctx, query, args...)
}
func (p *boundedPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	conn, err := Acquire(ctx, p.pool, p.budget)
	if err != nil {
		return nil, err
	}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	// Conn.Close waits for Rows.Close/EOF. At most pool.MaxOpenConns such
	// release goroutines can be live; request cancellation closes active rows.
	go conn.Close()
	return rows, nil
}
func (p *boundedPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	conn, err := Acquire(ctx, p.pool, p.budget)
	if err != nil {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return p.pool.QueryRowContext(cancelled, query, args...)
	}
	row := conn.QueryRowContext(ctx, query, args...)
	go conn.Close()
	return row
}
func (p *boundedPool) BeginTx(ctx context.Context, opts *sql.TxOptions) (gorm.ConnPool, error) {
	conn, err := Acquire(ctx, p.pool, p.budget)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, opts)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	lease := &leasedTx{Tx: tx, conn: conn, pool: p.pool}
	lease.stop = context.AfterFunc(ctx, lease.release)
	return lease, nil
}

type leasedTx struct {
	pool *sql.DB
	*sql.Tx
	conn *sql.Conn
	once sync.Once
	stop func() bool
}

var _ gorm.Tx = (*leasedTx)(nil)

func (t *leasedTx) GetDBConn() (*sql.DB, error) { return t.pool, nil }

func (t *leasedTx) release() { t.once.Do(func() { _ = t.conn.Close() }) }
func (t *leasedTx) Commit() error {
	err := t.Tx.Commit()
	if t.stop != nil {
		t.stop()
	}
	t.release()
	return err
}
func (t *leasedTx) Rollback() error {
	err := t.Tx.Rollback()
	if t.stop != nil {
		t.stop()
	}
	t.release()
	return err
}
