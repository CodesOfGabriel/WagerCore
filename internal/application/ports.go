package application

import (
	"context"
	"errors"
	"time"

	"junglegaming/internal/domain"
)

var (
	ErrNotFound       = errors.New("resource not found")
	ErrConflict       = errors.New("identity or payload conflict")
	ErrUnavailable    = errors.New("dependency temporarily unavailable")
	ErrWalletMismatch = errors.New("wallet player or currency does not match")
	ErrLeaseLost      = errors.New("work reservation is no longer owned")
	ErrInvalidCursor  = errors.New("invalid ledger cursor")
)

type Trace struct{ CorrelationID, CausationID string }
type IncomingMessage struct {
	Consumer, ID string
	Hash         []byte
}
type ReferenceSchedule struct {
	Attempts                  int
	Next, Expires, LeaseUntil time.Time
	LeaseID                   string
}

type Store interface {
	Identity(context.Context, string, string, string) (*domain.WagerTransaction, error)
	WalletForUpdate(context.Context, string) (*domain.Wallet, time.Time, error)
	CreateWallet(context.Context, *domain.Wallet) error
	SaveWallet(context.Context, *domain.Wallet) error
	ReserveTransaction(context.Context, *domain.WagerTransaction, Trace) (bool, error)
	SaveTransaction(context.Context, *domain.WagerTransaction, *ReferenceSchedule) error
	Transaction(context.Context, string) (*domain.WagerTransaction, error)
	ExternalTransaction(context.Context, string, string) (*domain.WagerTransaction, error)
	Reversed(context.Context, string) (bool, error)
	AddLedger(context.Context, domain.WalletLedgerEntry) error
	AddEvents(context.Context, []domain.Event) error
	ReserveInbox(context.Context, IncomingMessage) error
	CompleteInbox(context.Context, IncomingMessage, string) error
	ReferenceSchedule(context.Context, string) (ReferenceSchedule, error)
	TransactionTrace(context.Context, string) (Trace, error)
}

type UnitOfWork interface {
	Within(context.Context, func(Store) error) error
}

type LedgerPage struct {
	Entries    []domain.LedgerSnapshot
	NextCursor string
}

type Reconciliation struct {
	WalletID          string       `json:"walletId"`
	StoredBalance     domain.Money `json:"storedBalance"`
	CalculatedBalance domain.Money `json:"calculatedBalance"`
	Difference        domain.Money `json:"difference"`
	Consistent        bool         `json:"consistent"`
	CheckedEntries    int64        `json:"checkedEntries"`
}

type Reader interface {
	Wallet(context.Context, string) (*domain.Wallet, error)
	Transaction(context.Context, string) (*domain.WagerTransaction, error)
	ExternalTransaction(context.Context, string, string) (*domain.WagerTransaction, error)
	Ledger(context.Context, string, string, int) (LedgerPage, error)
	Reconcile(context.Context, string) (Reconciliation, error)
}

type Observer interface {
	Processed(string, string, time.Duration)
	Replay()
	Conflict()
	Retry(string)
	Reconciled(bool)
}

type Faults interface{ Hit(string, string) }
