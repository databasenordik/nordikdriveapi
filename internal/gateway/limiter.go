package gateway

import (
	"context"
	"database/sql"
	"time"

	"nordik-drive-api/internal/database"
)

type Decision struct {
	Allowed    bool
	RetryAfter int
}
type Limiter interface {
	Check(context.Context, string, string, int) (Decision, error)
}
type PostgresLimiter struct {
	Pool                       *sql.DB
	AcquireBudget, QueryBudget time.Duration
}

// The database clock defines a 60-second fixed window. The conditional upsert
// serializes competing replicas; exhausted windows never increment the count.
const limiterSQL = `WITH clock AS (
 SELECT date_trunc('minute', statement_timestamp()) AS start, statement_timestamp() AS now
), attempt AS (
 INSERT INTO rate_limits AS counters(policy,subject,window_start,count,expires_at)
 SELECT $1,$2,start,1,start+interval '2 minutes' FROM clock
 ON CONFLICT(policy,subject,window_start) DO UPDATE
 SET count=counters.count+1 WHERE counters.count<$3
 RETURNING count
)
SELECT EXISTS(SELECT 1 FROM attempt), greatest(1,ceil(extract(epoch FROM (start+interval '1 minute'-now))))::int FROM clock`

func (p PostgresLimiter) Check(ctx context.Context, policy, subject string, limit int) (Decision, error) {
	acquire, query := p.AcquireBudget, p.QueryBudget
	if acquire <= 0 {
		acquire = 2 * time.Second
	}
	if query <= 0 {
		query = 250 * time.Millisecond
	}
	conn, err := database.Acquire(ctx, p.Pool, acquire)
	if err != nil {
		return Decision{}, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, query)
	defer cancel()
	var result Decision
	err = conn.QueryRowContext(ctx, limiterSQL, policy, subject, limit).Scan(&result.Allowed, &result.RetryAfter)
	return result, err
}

// Cleanup is run by a finite Cloud Run Job under the gateway identity.
func Cleanup(ctx context.Context, pool *sql.DB) (int64, error) {
	result, err := pool.ExecContext(ctx, `DELETE FROM rate_limits WHERE ctid IN (SELECT ctid FROM rate_limits WHERE expires_at<statement_timestamp() ORDER BY expires_at LIMIT 1000)`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
