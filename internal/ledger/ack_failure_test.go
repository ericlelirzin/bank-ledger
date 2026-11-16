package ledger

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The connection wrapper and test coordinate through atomic flags because they
// run on different goroutines. This fault injection exists only in test code.
type ackFault struct{ armed, committed atomic.Bool }
type ackLossConn struct {
	net.Conn
	fault *ackFault
	drop  bool
}

func (c *ackLossConn) Write(p []byte) (int, error) {
	// pgx sends parameterless COMMIT using the simple-query protocol.
	if len(p) > 5 && p[0] == 'Q' && strings.EqualFold(string(p[5:len(p)-1]), "commit") && c.fault.armed.CompareAndSwap(true, false) {
		c.drop = true
	}
	return c.Conn.Write(p)
}
func (c *ackLossConn) Read(p []byte) (int, error) {
	if !c.drop {
		return c.Conn.Read(p)
	}
	// Consume the real server response through ReadyForQuery, proving that COMMIT
	// succeeded, but expose only a transport failure to pgx and the posting code.
	committed := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(c.Conn, header[:]); err != nil {
			return 0, err
		}
		size := int(binary.BigEndian.Uint32(header[1:])) - 4
		if size < 0 || size > 1<<20 {
			return 0, io.ErrUnexpectedEOF
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(c.Conn, payload); err != nil {
			return 0, err
		}
		if header[0] == 'C' && string(payload) == "COMMIT\x00" {
			committed = true
		}
		if header[0] == 'Z' {
			c.fault.committed.Store(committed)
			_ = c.Conn.Close()
			return 0, io.ErrUnexpectedEOF
		}
	}
}

// TestPostgresLostCommitAcknowledgment proves the server committed while pgx
// saw a transport error. An identical retry must return that journal and avoid
// a second balance change.
func TestPostgresLostCommitAcknowledgment(t *testing.T) {
	fault := &ackFault{}
	s := testStore(t, func(cfg *pgxpool.Config) {
		if cfg.ConnConfig.TLSConfig != nil {
			t.Skip("wire acknowledgment injection requires sslmode=disable")
		}
		dial := cfg.ConnConfig.DialFunc
		cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dial(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &ackLossConn{Conn: conn, fault: fault}, nil
		}
	})
	a, b, _ := setupAccounts(t, s)
	ctx := context.Background()
	tr := Transfer{Key: "lost-ack", From: a.ID, To: b.ID, Amount: 5}
	// Arm only after setup so the hidden response belongs to the target posting.
	fault.armed.Store(true)
	first, err := s.Transfer(ctx, tr)
	if err == nil || !fault.committed.Load() {
		t.Fatalf("fault did not hide a successful commit: %+v %v committed=%v", first, err, fault.committed.Load())
	}
	second, err := s.Transfer(ctx, tr)
	if err != nil || !second.Replayed || second.ID != first.ID {
		t.Fatalf("retry: %+v %v first=%+v", second, err, first)
	}
	bb, err := s.Account(ctx, b.ID)
	if err != nil || bb.Balance != 5 {
		t.Fatalf("ack loss duplicated money: %+v %v", bb, err)
	}
	var n int
	if err = s.DB.QueryRow(ctx, `SELECT count(*) FROM journals WHERE idempotency_key='lost-ack'`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}
