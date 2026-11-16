package ledger

import (
	"fmt"
	"math"
	"testing"
)

// Existing journals retain their request hashes across application upgrades.
// These fixed v0.2 values catch encoding changes that would break their retries.
func TestRequestHashCompatibility(t *testing.T) {
	posting := Posting{Key: "stable", Description: "persisted request", Entries: []Entry{{2, "credit", 7}, {1, "debit", 7}}}
	transfer := Transfer{Key: "stable", From: 2, To: 1, Amount: 7}
	for _, tc := range []struct {
		name     string
		transfer *Transfer
		want     string
	}{
		{"journal", nil, "9749bc192fd5eee1097b1723bb2dd18a5fe38e6fef7e4185f550ef76f7d0b440"},
		{"transfer", &transfer, "80841c3a0efee0fcf4788ee8d1be79c413af2d7673bb4317004c0ac4429ee620"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := fmt.Sprintf("%x", requestHash(posting, tc.transfer)); got != tc.want {
				t.Fatalf("persisted request hash changed: got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	valid := Posting{Key: "key", Entries: []Entry{{1, "debit", 100}, {2, "credit", 100}}}
	if e := Validate(valid); e != nil {
		t.Fatal(e)
	}
	tests := []Posting{
		{Entries: valid.Entries},
		{Key: "key", Entries: []Entry{{1, "debit", 100}, {2, "credit", 99}}},
		{Key: "key", Entries: []Entry{{1, "debit", 0}, {2, "credit", 0}}},
		{Key: "key", Entries: []Entry{{1, "debit", -1}, {2, "credit", -1}}},
		{Key: "key", Entries: []Entry{{0, "debit", 1}, {2, "credit", 1}}},
		{Key: "key", Entries: []Entry{{1, "other", 1}, {2, "credit", 1}}},
		// A wrapped signed sum must never make an unbalanced journal valid.
		{Key: "key", Entries: []Entry{{1, "debit", math.MaxInt64}, {1, "debit", math.MaxInt64}, {1, "debit", 3}, {2, "credit", 1}}},
	}
	for i, p := range tests {
		if Validate(p) == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	valid.Entries = []Entry{{1, "debit", math.MaxInt64}, {2, "debit", math.MaxInt64}, {3, "credit", math.MaxInt64}, {4, "credit", math.MaxInt64}}
	if e := Validate(valid); e != nil {
		t.Fatal(e)
	}
}
