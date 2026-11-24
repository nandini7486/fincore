package httpapi_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"testing"

	"github.com/nandini7486/fincore/backend/internal/auth"
	"github.com/nandini7486/fincore/backend/internal/money"
	"github.com/nandini7486/fincore/backend/internal/reconcile"
)

// A realistic burst of traffic through the real API: users paying each other
// at random, admins topping up at the same time, and a quarter of all
// payments double-sent concurrently with the same Idempotency-Key (a
// double-tapped button). Afterwards:
//   - the reconciler reports the books balance,
//   - every balance equals the value computed independently from the
//     responses,
//   - total user money equals total top-ups (nothing created or lost).
func TestStressRandomConcurrentTraffic(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}
	m := setupMoney(t)

	const (
		users           = 8
		paymentsPerUser = 30
		topUpsPerUser   = 3
		startBalance    = 50000 // ₹500.00 each, so some payments fail for lack of funds
	)
	type member struct {
		token, account string
	}
	members := make([]member, users)
	for i := range members {
		_, token := m.createUser(t, fmt.Sprintf("user%d@example.com", i), auth.RoleUser)
		members[i] = member{token: token, account: m.createAccount(t, token).AccountID}
	}

	var mu sync.Mutex
	expected := map[string]int64{} // account -> balance from successful responses
	var issued int64               // total money topped up
	succeeded, failedFunds, duplicates := 0, 0, 0
	apply := func(from, to string, amount int64) {
		mu.Lock()
		defer mu.Unlock()
		if from != "" {
			expected[from] -= amount
		}
		expected[to] += amount
	}

	topUp := func(account string, amount int64) {
		r := result(m.moneyPost(t, "/api/v1/accounts/"+account+"/topups", m.adminToken, newKey(), topUpBody(money.Format(amount))))
		if r.code != http.StatusCreated {
			t.Errorf("top-up = %d %s", r.code, r.errCode)
			return
		}
		apply("", account, amount)
		mu.Lock()
		issued += amount
		mu.Unlock()
	}
	for _, mb := range members {
		topUp(mb.account, startBalance)
	}

	var wg sync.WaitGroup
	// Admin keeps topping up while payments flow.
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewPCG(99, 1))
		for i := 0; i < users*topUpsPerUser; i++ {
			topUp(members[rng.IntN(users)].account, 1+rng.Int64N(20000))
		}
	}()

	for i := range members {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), 7)) // reproducible per user
			me := members[i]
			for n := 0; n < paymentsPerUser; n++ {
				to := members[(i+1+rng.IntN(users-1))%users].account
				amount := 1 + rng.Int64N(15000) // ₹0.01 to ₹150.00
				key, body := newKey(), payBody(me.account, to, money.Format(amount))

				copies := 1
				if rng.IntN(4) == 0 {
					copies = 2 // double-tap: same request sent twice at once
				}
				res := make([]*httpResult, copies)
				var cw sync.WaitGroup
				for c := range res {
					cw.Add(1)
					go func() {
						defer cw.Done()
						res[c] = result(m.moneyPost(t, "/api/v1/payments", me.token, key, body))
					}()
				}
				cw.Wait()

				fresh := 0
				for _, r := range res {
					if !r.replayed {
						fresh++
					}
				}
				if fresh != 1 {
					t.Errorf("key %s: %d copies processed, want exactly 1", key, fresh)
				}
				first := res[0]
				for _, r := range res[1:] {
					if r.code != first.code || r.payment.PaymentID+r.paymentID != first.payment.PaymentID+first.paymentID {
						t.Errorf("duplicate got %d/%s, original %d/%s", r.code, r.payment.PaymentID+r.paymentID, first.code, first.payment.PaymentID+first.paymentID)
					}
				}

				mu.Lock()
				duplicates += copies - 1
				mu.Unlock()
				switch {
				case first.code == http.StatusCreated:
					apply(me.account, to, amount)
					mu.Lock()
					succeeded++
					mu.Unlock()
				case first.code == http.StatusUnprocessableEntity && first.errCode == "INSUFFICIENT_FUNDS":
					mu.Lock()
					failedFunds++
					mu.Unlock()
				default:
					t.Errorf("payment = %d %s", first.code, first.errCode)
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	rep, err := reconcile.Run(context.Background(), m.db.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("books do not balance after the burst: %+v", rep)
	}

	var total int64
	for _, mb := range members {
		var got int64
		if err := m.db.Owner.QueryRow(context.Background(),
			`SELECT balance FROM accounts WHERE id = $1`, mb.account).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != expected[mb.account] {
			t.Errorf("account %s balance = %d, expected %d from responses", mb.account, got, expected[mb.account])
		}
		total += got
	}
	if total != issued {
		t.Errorf("users hold %d in total, but %d was issued", total, issued)
	}
	var treasury int64
	if err := m.db.Owner.QueryRow(context.Background(),
		`SELECT balance FROM accounts WHERE type = 'TREASURY'`).Scan(&treasury); err != nil {
		t.Fatal(err)
	}
	if treasury != -issued {
		t.Errorf("treasury = %d, want %d", treasury, -issued)
	}
	if succeeded == 0 || failedFunds == 0 {
		t.Errorf("traffic too one-sided to be meaningful: %d succeeded, %d insufficient funds", succeeded, failedFunds)
	}
	t.Logf("%d payments succeeded, %d failed for funds, %d duplicate copies replayed, %s issued, %d ledger entries; books balance",
		succeeded, failedFunds, duplicates, money.Format(issued), rep.LedgerEntries)
}
