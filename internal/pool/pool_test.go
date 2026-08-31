package pool

import (
	"errors"
	"testing"
)

func TestBytesString(t *testing.T) {
	for _, tc := range []struct {
		in   Bytes
		want string
	}{
		{512, "512B"},
		{2 * KiB, "2.00KiB"},
		{3 * MiB, "3.00MiB"},
		{4 * GiB, "4.00GiB"},
		{8 * TiB, "8.00TiB"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Errorf("Bytes(%d).String() = %q, want %q", int64(tc.in), got, tc.want)
		}
	}
}

func TestFreeAndValidate(t *testing.T) {
	for _, tc := range []struct {
		name     string
		acct     Accounting
		wantFree Bytes
		wantErr  error
	}{
		{
			name:     "balances",
			acct:     Accounting{Capacity: 8 * TiB, Compute: 1 * TiB, Donated: 2 * TiB, Borrowed: 4 * TiB},
			wantFree: 1 * TiB,
		},
		{
			name:     "fully allocated is still valid",
			acct:     Accounting{Capacity: 4 * TiB, Compute: 1 * TiB, Donated: 1 * TiB, Borrowed: 2 * TiB},
			wantFree: 0,
		},
		{
			name:     "overcommitted is a defect",
			acct:     Accounting{Capacity: 4 * TiB, Compute: 2 * TiB, Donated: 2 * TiB, Borrowed: 1 * TiB},
			wantFree: -1 * TiB,
			wantErr:  ErrOvercommit,
		},
		{
			name:    "negative pool is a defect",
			acct:    Accounting{Capacity: 4 * TiB, Borrowed: -1},
			wantErr: ErrNegative,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.wantErr == nil || !errors.Is(tc.wantErr, ErrNegative) {
				if got := tc.acct.Free(); got != tc.wantFree {
					t.Errorf("Free() = %s, want %s", got, tc.wantFree)
				}
			}
			err := tc.acct.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLendRefusesRatherThanOvercommits(t *testing.T) {
	a := Accounting{Capacity: 10 * GiB, Compute: 2 * GiB, Donated: 2 * GiB}

	if err := a.Lend(4 * GiB); err != nil {
		t.Fatalf("Lend(4GiB) = %v, want nil", err)
	}
	if got := a.Borrowed; got != 4*GiB {
		t.Fatalf("Borrowed = %s, want 4.00GiB", got)
	}
	if got := a.Free(); got != 2*GiB {
		t.Fatalf("Free() = %s, want 2.00GiB", got)
	}

	// The whole point of node-side authority: the excess is refused, not taken.
	if err := a.Lend(4 * GiB); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("Lend beyond free = %v, want ErrInsufficient", err)
	}
	if got := a.Borrowed; got != 4*GiB {
		t.Fatalf("refused Lend mutated Borrowed to %s", got)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate after refusal = %v, want nil", err)
	}
}

func TestLendAndReturnRejectNegative(t *testing.T) {
	a := Accounting{Capacity: 4 * GiB}
	if err := a.Lend(-1); !errors.Is(err, ErrNegative) {
		t.Errorf("Lend(-1) = %v, want ErrNegative", err)
	}
	if err := a.Return(-1); !errors.Is(err, ErrNegative) {
		t.Errorf("Return(-1) = %v, want ErrNegative", err)
	}
	if err := a.Return(1 * GiB); !errors.Is(err, ErrOverRelease) {
		t.Errorf("Return more than borrowed = %v, want ErrOverRelease", err)
	}
}

func TestReturnFreesCapacity(t *testing.T) {
	a := Accounting{Capacity: 8 * GiB, Borrowed: 6 * GiB}
	if err := a.Return(6 * GiB); err != nil {
		t.Fatalf("Return = %v, want nil", err)
	}
	if a.Borrowed != 0 || a.Free() != 8*GiB {
		t.Fatalf("after Return: borrowed %s free %s, want 0 and 8.00GiB", a.Borrowed, a.Free())
	}
}

func TestReclaimableExcludesDonated(t *testing.T) {
	// Donated capacity is never handed back, so it bounds how far a node can
	// be recovered no matter how much pressure it is under.
	a := Accounting{Capacity: 8 * TiB, Compute: 1 * TiB, Donated: 3 * TiB, Borrowed: 2 * TiB}
	if got := a.Reclaimable(); got != 2*TiB {
		t.Errorf("Reclaimable() = %s, want 2.00TiB", got)
	}
}

func TestCanLend(t *testing.T) {
	a := Accounting{Capacity: 4 * GiB, Borrowed: 3 * GiB}
	if !a.CanLend(1 * GiB) {
		t.Error("CanLend(1GiB) = false, want true")
	}
	if a.CanLend(2 * GiB) {
		t.Error("CanLend(2GiB) = true, want false")
	}
	if a.CanLend(-1) {
		t.Error("CanLend(-1) = true, want false")
	}
}

func TestOverReleaseIsNotANegativeInput(t *testing.T) {
	// Returning more than was lent is an accounting failure, not a bad
	// argument, and a caller has to be able to tell those apart.
	a := Accounting{Capacity: 4 * GiB, Borrowed: 1 * GiB}
	err := a.Return(2 * GiB)
	if !errors.Is(err, ErrOverRelease) {
		t.Fatalf("Return(2GiB) with 1GiB borrowed = %v, want ErrOverRelease", err)
	}
	if errors.Is(err, ErrNegative) {
		t.Error("over-release also matched ErrNegative, which hides the distinction")
	}
	if a.Borrowed != 1*GiB {
		t.Errorf("refused Return mutated Borrowed to %s", a.Borrowed)
	}
}

func TestValidateReportsTheSameFieldEveryTime(t *testing.T) {
	// Two bad fields must not produce a message that changes between runs.
	a := Accounting{Capacity: -1, Compute: -1}
	first := a.Validate().Error()
	for i := 0; i < 50; i++ {
		if got := a.Validate().Error(); got != first {
			t.Fatalf("Validate() message varies between calls: %q then %q", first, got)
		}
	}
}
