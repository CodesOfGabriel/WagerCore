//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"junglegaming/internal/application"
	"junglegaming/internal/domain"
)

func TestAuthenticatedWalletAndIdempotency(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal, provider, other := token(t, "internal-service"), token(t, "provider-a"), token(t, "provider-b")
	for _, bearer := range []string{"", "invalid.signature.token", token(t, "provider-wrong-audience")} {
		expect(t, p, "POST", "/wallets", bearer, "", nil, 401, nil)
	}
	expect(t, p, "POST", "/wallets", provider, "", nil, 403, nil)
	expect(t, p, "POST", "/wagering/transactions", internal, "", nil, 403, nil)
	expired := token(t, "provider-expiring")
	time.Sleep(2100 * time.Millisecond)
	expect(t, p, "POST", "/wagering/transactions", expired, "", nil, 401, nil)
	if db.count(t, "SELECT count(*) FROM wallets") != 0 {
		t.Fatal("unauthorized request changed database")
	}
	w := openAPIWallet(t, p, internal, "100.00")
	expect(t, p, "POST", "/wallets", internal, "", map[string]any{"playerId": w.player, "initialBalance": money("100.00")}, 409, nil)
	z := openAPIWallet(t, p, internal, "0.00")
	db.assertBalance(t, w, 10000, 1)
	db.assertBalance(t, z, 0, 1)
	if db.count(t, "SELECT count(*) FROM outbox") != 2 || db.count(t, "SELECT count(*) FROM wager_transactions WHERE wallet_id=$1", z.id) != 0 {
		t.Fatal("opening artifacts do not match positive/zero policy")
	}
	var openingProvider *string
	if err := db.conn.QueryRow(db.ctx, "SELECT provider_id FROM wager_transactions WHERE wallet_id=$1", w.id).Scan(&openingProvider); err != nil || openingProvider != nil {
		t.Fatalf("internal opening provider=%v error=%v", openingProvider, err)
	}
	r := request(w, domain.Bet, "25.00")
	key := "custom-key-" + newID()
	result := wager(t, p, provider, r, key, 200)
	if result.IdempotentReplay || result.Balance == nil || *result.Balance != money("75.00") {
		t.Fatalf("invalid bet: %+v", result)
	}
	wager(t, p, provider, request(w, domain.Bet, "10.00"), newID(), 200)
	replay := wager(t, p, provider, r, key, 200)
	if !replay.IdempotentReplay || replay.TransactionID != result.TransactionID || *replay.Balance != money("75.00") {
		t.Fatalf("original result was not replayed: %+v", replay)
	}
	changed := r
	changed.Money = money("26.00")
	wager(t, p, provider, changed, key, 409)
	wager(t, p, provider, r, newID(), 409)
	changed = r
	changed.ExternalTransactionID = newID()
	wager(t, p, provider, changed, key, 409)
	expect(t, p, "POST", "/wagering/transactions", other, key, r, 403, nil)
	expect(t, p, "GET", "/wagering/transactions/"+result.TransactionID, other, "", nil, 404, nil)
	expect(t, p, "GET", "/providers/provider-a/wagering/transactions/"+r.ExternalTransactionID, other, "", nil, 404, nil)
	expect(t, p, "GET", "/wagering/transactions/"+result.TransactionID, provider, "", nil, 200, nil)
	expect(t, p, "GET", "/providers/provider-a/wagering/transactions/"+r.ExternalTransactionID, provider, "", nil, 200, nil)
	expect(t, p, "GET", "/wallets/"+w.id, provider, "", nil, 403, nil)
	expect(t, p, "GET", "/wallets/invalid", internal, "", nil, 400, nil)
	expect(t, p, "GET", "/wallets/"+newID(), internal, "", nil, 404, nil)
	for _, kind := range []domain.TransactionKind{domain.Opening, domain.Bet, domain.Loss} {
		invalid := request(w, kind, "0.00")
		if kind == domain.Loss {
			invalid.Money = money("1.00")
		}
		wager(t, p, provider, invalid, newID(), 400)
	}
	raw, _ := json.Marshal(r)
	raw = bytes.ReplaceAll(raw, []byte(`"25.00"`), []byte(`25.00`))
	expect(t, p, "POST", "/wagering/transactions", provider, key, raw, 400, nil)
	db.assertBalance(t, w, 6500, 3)
	rejected := request(w, domain.Bet, "80.00")
	rejection := wager(t, p, provider, rejected, newID(), 422)
	if rejection.FailureCode != domain.InsufficientFunds {
		t.Fatalf("rejection=%+v", rejection)
	}
	if db.count(t, "SELECT count(*) FROM outbox WHERE payload->'data'->>'transactionId'=$1", rejection.TransactionID) != 1 {
		t.Fatal("rejection requires one transaction event")
	}
	var reconciliation application.Reconciliation
	expect(t, p, "POST", "/wallets/"+w.id+"/reconciliation", internal, "", nil, 200, &reconciliation)
	if !reconciliation.Consistent || reconciliation.CheckedEntries != 3 || reconciliation.Difference.MinorUnits() != 0 {
		t.Fatalf("reconciliation=%+v", reconciliation)
	}
	seen := map[string]bool{}
	path := "/wallets/" + w.id + "/ledger?limit=1"
	for path != "" {
		var page struct {
			Entries    []struct{ ID string }
			NextCursor string
		}
		expect(t, p, "GET", path, internal, "", nil, 200, &page)
		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Fatal("duplicate ledger page")
			}
			seen[entry.ID] = true
		}
		path = ""
		if page.NextCursor != "" {
			path = "/wallets/" + w.id + "/ledger?limit=1&cursor=" + page.NextCursor
		}
	}
	if len(seen) != 3 {
		t.Fatalf("paginated %d entries", len(seen))
	}
	expect(t, p, "GET", "/wallets/"+w.id+"/ledger?cursor=invalid", internal, "", nil, 400, nil)
	metrics := expect(t, p, "GET", "/metrics", internal, "", nil, 200, nil)
	if !strings.Contains(string(metrics), "wager_idempotent_replays_total 1") || !strings.Contains(string(metrics), "wager_wallet_lock_duration_seconds") {
		t.Fatalf("missing business metrics: %s", metrics)
	}
	expect(t, p, "GET", "/metrics", provider, "", nil, 403, nil)
}

func TestFinancialOperationsAndReversals(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	send := func(r application.WagerRequest, status int) application.WagerResult {
		return wager(t, p, provider, r, newID(), status)
	}
	bet := request(w, domain.Bet, "25.00")
	send(bet, 200)
	win := request(w, domain.Win, "40.00")
	win.ReferenceExternalTransactionID = bet.ExternalTransactionID
	send(win, 200)
	loss := request(w, domain.Loss, "0.00")
	send(loss, 200)
	db.assertBalance(t, w, 11500, 3)
	refund := request(w, domain.Refund, "25.00")
	refund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	send(refund, 200)
	rollbackBet := request(w, domain.Rollback, "25.00")
	rollbackBet.ReferenceExternalTransactionID = bet.ExternalTransactionID
	if send(rollbackBet, 422).FailureCode != domain.AlreadyReversed {
		t.Fatal("refund and rollback both reversed same bet")
	}
	rollbackRefund := request(w, domain.Rollback, "25.00")
	rollbackRefund.ReferenceExternalTransactionID = refund.ExternalTransactionID
	send(rollbackRefund, 200)
	rollbackWin := request(w, domain.Rollback, "40.00")
	rollbackWin.ReferenceExternalTransactionID = win.ExternalTransactionID
	send(rollbackWin, 200)
	db.assertBalance(t, w, 7500, 6)
	for _, tt := range []struct {
		name string
		edit func(*application.WagerRequest)
		code domain.FailureCode
	}{
		{"wrong amount", func(r *application.WagerRequest) { r.Money = money("24.00") }, domain.AmountMismatch},
		{"wrong round", func(r *application.WagerRequest) { r.RoundID = "other-round" }, domain.ReferenceMismatch},
		{"wrong kind", func(r *application.WagerRequest) { r.ReferenceExternalTransactionID = loss.ExternalTransactionID }, domain.ReferenceMismatch},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := request(w, domain.Refund, "25.00")
			r.ReferenceExternalTransactionID = bet.ExternalTransactionID
			tt.edit(&r)
			if send(r, 422).FailureCode != tt.code {
				t.Fatal("incorrect rejection")
			}
		})
	}
	v := openAPIWallet(t, p, internal, "0.00")
	credit := request(v, domain.Win, "10.00")
	send(credit, 200)
	send(request(v, domain.Bet, "10.00"), 200)
	reverseCredit := request(v, domain.Rollback, "10.00")
	reverseCredit.ReferenceExternalTransactionID = credit.ExternalTransactionID
	if send(reverseCredit, 422).FailureCode != domain.ReversalInsufficientFunds {
		t.Fatal("reversal insufficient funds must have distinct code")
	}
	db.assertBalance(t, v, 0, 3)
	maximum := db.openWallet(t, math.MaxInt64)
	if send(request(maximum, domain.Win, "0.01"), 422).FailureCode != domain.BalanceOverflow {
		t.Fatal("balance overflow not audited")
	}
	db.assertBalance(t, maximum, math.MaxInt64, 1)
}

func TestAtomicRollbackWhenOutboxFails(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, p, internal, "100.00")
	_, err := db.conn.Exec(db.ctx, `CREATE FUNCTION reject_test_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected outbox failure'; END $$;
 CREATE TRIGGER reject_test_event BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_test_event()`)
	if err != nil {
		t.Fatal(err)
	}
	wager(t, p, provider, request(w, domain.Bet, "25.00"), newID(), 503)
	expect(t, p, "POST", "/wallets", internal, "", map[string]any{"playerId": newID(), "initialBalance": money("1.00")}, 503, nil)
	db.assertBalance(t, w, 10000, 1)
	if db.count(t, "SELECT count(*) FROM wallets") != 1 || db.count(t, "SELECT count(*) FROM wager_transactions") != 1 || db.count(t, "SELECT count(*) FROM outbox") != 2 {
		t.Fatal("outbox failure left partial financial state")
	}
	found := false
	for _, entry := range p.entries() {
		if entry["msg"] == "request_unavailable" && entry["stage"] == "process_wager" && entry["errorCategory"] == "database" && entry["sqlState"] == "P0001" && entry["walletId"] == w.id {
			found = true
		}
	}
	if !found {
		t.Fatal("database failure lacks stage, wallet and SQLSTATE in logs")
	}

}

func TestThreeProcessesConcurrentBets(t *testing.T) {
	db := newDatabase(t, true)
	servers := []*serverProcess{startServer(t, db), startServer(t, db), startServer(t, db)}
	internal, provider := token(t, "internal-service"), token(t, "provider-a")
	w := openAPIWallet(t, servers[0], internal, "100.00")
	r := request(w, domain.Bet, "10.00")
	key := newID()
	type outcome struct {
		status int
		body   []byte
		err    error
	}
	results := make(chan outcome, 50)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i := range 50 {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			<-start
			status, body, err := call(servers[i%3].URL, "POST", "/wagering/transactions", provider, key, r)
			results <- outcome{status, body, err}
		}(i)
	}
	close(start)
	group.Wait()
	close(results)
	replays := 0
	id := ""
	for result := range results {
		if result.err != nil || result.status != 200 {
			t.Fatalf("concurrent response=%+v %s", result, result.body)
		}
		var body application.WagerResult
		if json.Unmarshal(result.body, &body) != nil {
			t.Fatal("invalid response")
		}
		if body.IdempotentReplay {
			replays++
		}
		if id != "" && id != body.TransactionID {
			t.Fatal("multiple transactions")
		}
		id = body.TransactionID
	}
	if replays != 49 {
		t.Fatalf("replays=%d", replays)
	}
	db.assertBalance(t, w, 9000, 2)
	if db.count(t, "SELECT count(*) FROM wallet_ledger WHERE wallet_id=$1", w.id) != 2 || db.count(t, "SELECT count(*) FROM outbox WHERE payload->'data'->>'transactionId'=$1", id) != 2 {
		t.Fatal("duplicate financial artifacts")
	}
	v := openAPIWallet(t, servers[0], internal, "100.00")
	results = make(chan outcome, 2)
	for i := range 2 {
		go func(i int) {
			status, body, err := call(servers[i].URL, "POST", "/wagering/transactions", provider, newID(), request(v, domain.Bet, "80.00"))
			results <- outcome{status, body, err}
		}(i)
	}
	statuses := map[int]int{}
	for range 2 {
		o := <-results
		if o.err != nil {
			t.Fatal(o.err)
		}
		statuses[o.status]++
	}
	if statuses[200] != 1 || statuses[422] != 1 {
		t.Fatalf("outcomes=%v", statuses)
	}
	db.assertBalance(t, v, 2000, 2)
	tx, err := db.conn.Begin(db.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(db.ctx)
	if _, err := tx.Exec(db.ctx, "SELECT id FROM wallets WHERE id=$1 FOR UPDATE", w.id); err != nil {
		t.Fatal(err)
	}
	wager(t, servers[2], provider, request(v, domain.Bet, "1.00"), newID(), 200)
	if err := tx.Rollback(db.ctx); err != nil {
		t.Fatal(err)
	}
	for _, server := range servers {
		server.stop(t)
	}
	restarted := startServer(t, db)
	replay := wager(t, restarted, provider, r, key, 200)
	if !replay.IdempotentReplay || replay.Balance.MinorUnits() != 9000 {
		t.Fatalf("restart replay=%+v", replay)
	}
}

func TestReconciliationReportsDivergenceWithoutRepair(t *testing.T) {
	db := newDatabase(t, true)
	p := startServer(t, db)
	internal := token(t, "internal-service")
	w := openAPIWallet(t, p, internal, "100.00")
	// Corrupção deliberada no schema isolado permite conferir o diagnóstico sem relaxar a aplicação.
	_, err := db.conn.Exec(db.ctx, `ALTER TABLE wallets DISABLE TRIGGER USER;
 UPDATE wallets SET balance_minor=balance_minor+1;
 ALTER TABLE wallets ENABLE TRIGGER USER`)
	if err != nil {
		t.Fatal(err)
	}
	var result application.Reconciliation
	expect(t, p, "POST", "/wallets/"+w.id+"/reconciliation", internal, "", nil, 200, &result)
	if result.Consistent || result.Difference.MinorUnits() != 1 || result.CheckedEntries != 1 {
		t.Fatalf("divergence not reported: %+v", result)
	}
	var stored int64
	if err := db.conn.QueryRow(db.ctx, "SELECT balance_minor FROM wallets WHERE id=$1", w.id).Scan(&stored); err != nil || stored != 10001 {
		t.Fatalf("reconciliation changed wallet: stored=%d err=%v", stored, err)
	}
	metrics := expect(t, p, "GET", "/metrics", internal, "", nil, 200, nil)
	if !strings.Contains(string(metrics), "wager_reconciliation_mismatches_total 1") {
		t.Fatal("missing mismatch metric")
	}
}
