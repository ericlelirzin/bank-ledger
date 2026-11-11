package ledger

import (
	"context"
	"fmt"
	"testing"
)

// Bulk SQL isolates database trigger work from Go's per-line round trips.
// Run explicitly with TEST_DATABASE_URL; this compares the unchanged v1 SQL
// with the forward-migrated v2 schema using the same hardware and workload.
func BenchmarkPostgresJournalValidation(b *testing.B) {
	for _, version := range []int{1, 2} {
		for _, lines := range []int{100, 1000} {
			b.Run(fmt.Sprintf("v%d/lines_%d", version, lines), func(b *testing.B) {
				s := testStoreAtVersion(b, version)
				a, _, _ := setupAccounts(b, s)
				ctx := context.Background()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					tx, e := s.DB.Begin(ctx)
					if e != nil {
						b.Fatal(e)
					}
					var id int64
					e = tx.QueryRow(ctx, `INSERT INTO journals(idempotency_key,request_hash,description) VALUES($1,'x','') RETURNING id`, fmt.Sprintf("bench-%d", i)).Scan(&id)
					if e == nil {
						_, e = tx.Exec(ctx, `INSERT INTO entries SELECT $1,g,$2,CASE WHEN g%2=0 THEN 'debit' ELSE 'credit' END,1 FROM generate_series(1,$3::integer) g`, id, a.ID, lines)
					}
					// Every journal has net zero on an existing funded account; no overdraft
					// or overflow confounds validation cost. The other fixture account is idle.
					if e == nil {
						e = tx.Commit(ctx)
					} else {
						_ = tx.Rollback(ctx)
					}
					if e != nil {
						b.Fatal(e)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(lines), "lines/op")
			})
		}
	}
}
