package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func testPool(t *testing.T) (*boundedPool, sqlmock.Sqlmock) {
	t.Helper()
	pool, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	return &boundedPool{pool: pool, budget: 20 * time.Millisecond}, mock
}
func available(t *testing.T, pool *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal("operation retained its connection")
	}
	connection.Close()
}

func TestRowsHoldConnectionUntilClosedAndThenRelease(t *testing.T) {
	p, mock := testPool(t)
	mock.ExpectQuery("SELECT fixture").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(1).AddRow(2))
	rows, err := p.QueryContext(context.Background(), "SELECT fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing first row")
	}
	var value int
	if err := rows.Scan(&value); err != nil || value != 1 {
		t.Fatal("row stream changed")
	}
	if _, err := Acquire(context.Background(), p.pool, 5*time.Millisecond); err == nil {
		t.Fatal("connection reused while rows were still open")
	}
	rows.Close()
	available(t, p.pool)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestRowExecAndTransactionsReleaseConnections(t *testing.T) {
	p, mock := testPool(t)
	mock.ExpectQuery("SELECT fixture").WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow(7))
	var value int
	if err := p.QueryRowContext(context.Background(), "SELECT fixture").Scan(&value); err != nil || value != 7 {
		t.Fatal("row scan changed")
	}
	available(t, p.pool)
	mock.ExpectExec("UPDATE fixture").WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := p.ExecContext(context.Background(), "UPDATE fixture"); err != nil {
		t.Fatal(err)
	}
	available(t, p.pool)
	for _, commit := range []bool{false, true} {
		mock.ExpectBegin()
		tx, err := p.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if commit {
			mock.ExpectCommit()
			err = tx.(*leasedTx).Commit()
		} else {
			mock.ExpectRollback()
			err = tx.(*leasedTx).Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		available(t, p.pool)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestPoolErrorsAndCancelledTransactions(t *testing.T) {
	p, mock := testPool(t)
	mock.ExpectQuery("SELECT broken").WillReturnError(errors.New("synthetic failure"))
	if _, err := p.QueryContext(context.Background(), "SELECT broken"); err == nil {
		t.Fatal("query error lost")
	}
	available(t, p.pool)
	mock.ExpectBegin().WillReturnError(errors.New("begin failed"))
	if _, err := p.BeginTx(context.Background(), nil); err == nil {
		t.Fatal("begin error lost")
	}
	available(t, p.pool)
	ctx, cancel := context.WithCancel(context.Background())
	mock.ExpectBegin()
	_, err := p.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectRollback()
	cancel()
	deadline := time.Now().Add(time.Second)
	for p.pool.Stats().InUse != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.pool.Stats().InUse != 0 {
		t.Fatal("cancelled transaction retained its connection")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	closed, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.QueryContext(closed, "SELECT denied"); err == nil {
		t.Fatal("cancelled query acquired connection")
	}
	if _, err := p.ExecContext(closed, "UPDATE denied"); err == nil {
		t.Fatal("cancelled write acquired connection")
	}
	if _, err := p.BeginTx(closed, nil); err == nil {
		t.Fatal("cancelled transaction acquired connection")
	}
	if err := p.QueryRowContext(closed, "SELECT denied").Scan(new(int)); err == nil {
		t.Fatal("cancelled row query admitted")
	}
}
