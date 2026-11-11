package ledger

import (
	"math"
	"testing"
)

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
