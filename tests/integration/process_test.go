//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"junglegaming/internal/application"
	"junglegaming/internal/bootstrap"
	"junglegaming/internal/domain"
)

func issuerURL() string {
	if v := os.Getenv("TEST_OIDC_ISSUER_URL"); v != "" {
		return v
	}
	return "http://localhost:58081/realms/wagercore"
}
func brokerEndpoint() string {
	if v := os.Getenv("TEST_SQS_ENDPOINT"); v != "" {
		return v
	}
	return "http://localhost:54566"
}

func TestServerProcess(t *testing.T) {
	if os.Getenv("WAGER_TEST_PROCESS") != "1" {
		return
	}
	bootstrap.New().Run()
}

type serverProcess struct {
	cmd  *exec.Cmd
	URL  string
	done chan struct{}
	err  error
	mu   sync.Mutex
	logs []map[string]any
}

func startServer(t *testing.T, db *database, overrides ...string) *serverProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &serverProcess{done: make(chan struct{})}
	p.cmd = exec.Command(executable, "-test.run=^TestServerProcess$")
	env := map[string]string{}
	for _, value := range os.Environ() {
		k, v, _ := strings.Cut(value, "=")
		env[k] = v
	}
	for _, value := range append([]string{
		"WAGER_TEST_PROCESS=1", "APP_ENV=test", "HTTP_ADDR=127.0.0.1:0", "DATABASE_URL=" + db.url(),
		"OIDC_ISSUER_URL=" + issuerURL(), "OIDC_JWKS_URL=" + issuerURL() + "/protocol/openid-connect/certs", "OIDC_AUDIENCE=wagercore-api",
		"SQS_ENDPOINT=" + brokerEndpoint(), "AWS_REGION=us-east-1", "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test",
		"WORKER_ROLES=none", "WORKER_INTERVAL=20ms", "LEASE_DURATION=500ms", "RETRY_BASE=50ms", "REFERENCE_TTL=10s", "REFERENCE_MAX_ATTEMPTS=10",
		"SQS_VISIBILITY_TIMEOUT=1", "FAULT_POINT=", "FAULT_TRANSACTION=",
	}, overrides...) {
		k, v, _ := strings.Cut(value, "=")
		env[k] = v
	}
	for k, v := range env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	output, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.cmd.Stderr = p.cmd.Stdout
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			var entry map[string]any
			if json.Unmarshal(scanner.Bytes(), &entry) != nil {
				continue
			}
			p.mu.Lock()
			p.logs = append(p.logs, entry)
			p.mu.Unlock()
			if entry["msg"] == "http_started" {
				address <- entry["address"].(string)
			}
		}
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() { p.stop(t) })
	select {
	case addr := <-address:
		p.URL = "http://" + addr
	case <-p.done:
		t.Fatalf("server exited: %v; logs=%v", p.err, p.entries())
	case <-time.After(25 * time.Second):
		t.Fatalf("server startup timed out; logs=%v", p.entries())
	}
	return p
}
func (p *serverProcess) entries() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]any(nil), p.logs...)
}
func (p *serverProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
		if p.err != nil {
			t.Errorf("graceful shutdown: %v; logs=%v", p.err, p.entries())
		}
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Error("server shutdown timed out")
	}
}
func (p *serverProcess) crashed(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("fault did not terminate process: %v", p.entries())
	}
	if exit, ok := p.err.(*exec.ExitError); !ok || exit.ExitCode() != 97 {
		t.Fatalf("unexpected fault exit: %v; logs=%v", p.err, p.entries())
	}
}

var apiClient = &http.Client{Timeout: 15 * time.Second}

func token(t *testing.T, client string) string {
	t.Helper()
	response, err := apiClient.PostForm(issuerURL()+"/protocol/openid-connect/token", url.Values{"grant_type": {"client_credentials"}, "client_id": {client}, "client_secret": {client + "-local-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&body) != nil || body.AccessToken == "" {
		t.Fatalf("client credentials failed for %s: status=%d", client, response.StatusCode)
	}
	return body.AccessToken
}
func call(base, method, path, bearer, key string, body any) (int, []byte, error) {
	var payload []byte
	var err error
	if raw, ok := body.([]byte); ok {
		payload = raw
	} else if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequest(method, base+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	response, err := apiClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, data, err
}
func expect(t *testing.T, p *serverProcess, method, path, bearer, key string, body any, status int, target any) []byte {
	t.Helper()
	actual, data, err := call(p.URL, method, path, bearer, key, body)
	if err != nil || actual != status {
		t.Fatalf("%s %s: status=%d body=%s err=%v; logs=%v", method, path, actual, data, err, p.entries())
	}
	if target != nil {
		if err := json.Unmarshal(data, target); err != nil {
			t.Fatalf("decode %s: %v", data, err)
		}
	}
	return data
}
func money(amount string) domain.Money {
	value, err := domain.NewMoney(amount, domain.BRL)
	if err != nil {
		panic(err)
	}
	return value
}
func openAPIWallet(t *testing.T, p *serverProcess, bearer, amount string) wallet {
	t.Helper()
	var result struct {
		ID, PlayerID string
		Balance      domain.Money
		Version      int64
	}
	player := newID()
	expect(t, p, "POST", "/wallets", bearer, "", map[string]any{"playerId": player, "initialBalance": money(amount)}, 201, &result)
	if result.Version != 1 || result.Balance != money(amount) {
		t.Fatalf("invalid opening: %+v", result)
	}
	return wallet{id: result.ID, player: result.PlayerID}
}
func request(w wallet, kind domain.TransactionKind, amount string) application.WagerRequest {
	return application.WagerRequest{ProviderID: "provider-a", ExternalTransactionID: newID(), PlayerID: w.player, WalletID: w.id, RoundID: "round-1", GameID: "game-1", Kind: kind, Money: money(amount)}
}
func wager(t *testing.T, p *serverProcess, bearer string, r application.WagerRequest, key string, status int) application.WagerResult {
	t.Helper()
	var result application.WagerResult
	expect(t, p, "POST", "/wagering/transactions", bearer, key, r, status, &result)
	return result
}
func eventually(t *testing.T, timeout time.Duration, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatal("timed out: " + description)
}
func (db *database) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.conn.QueryRow(db.ctx, query, args...).Scan(&n); err != nil {
		t.Fatal(fmt.Errorf("count: %w", err))
	}
	return n
}
