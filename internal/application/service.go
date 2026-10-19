package application

import (
	"context"
	"errors"
	"time"

	"junglegaming/internal/domain"
	"junglegaming/internal/identifier"
)

type ReferencePolicy struct {
	TTL, RetryBase time.Duration
	MaxAttempts    int
}

type Service struct {
	uow      UnitOfWork
	Reader   Reader
	observer Observer
	faults   Faults
	policy   ReferencePolicy
}

func NewService(uow UnitOfWork, reader Reader, observer Observer, faults Faults, policy ReferencePolicy) *Service {
	return &Service{uow: uow, Reader: reader, observer: observer, faults: faults, policy: policy}
}

func (s *Service) OpenWallet(ctx context.Context, playerID string, balance domain.Money, trace Trace) (*domain.Wallet, error) {
	started := time.Now()
	id, err := identifier.New()
	if err != nil {
		return nil, err
	}
	wallet, err := domain.NewWallet(id, playerID, balance, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	err = s.uow.Within(ctx, func(store Store) error {
		if err := store.CreateWallet(ctx, wallet); err != nil {
			return err
		}
		if balance.MinorUnits() == 0 {
			return nil
		}
		id, err := identifier.New()
		if err != nil {
			return err
		}
		opening, err := domain.NewOpeningTransaction(id, wallet)
		if err != nil {
			return err
		}
		at := wallet.Snapshot().CreatedAt
		if err := opening.Process(balance, "", at); err != nil {
			return err
		}
		reserved, err := store.ReserveTransaction(ctx, opening, trace)
		if err != nil {
			return err
		}
		if !reserved {
			return ErrConflict
		}
		zero, _ := domain.ZeroMoney(balance.Currency())
		return s.recordMovement(ctx, store, opening, wallet, domain.Credit, zero, trace, at)
	})
	if err != nil {
		return nil, err
	}
	if balance.MinorUnits() > 0 {
		s.observer.Processed(string(domain.Opening), string(domain.Processed), time.Since(started))
	}
	return wallet, nil
}

func (s *Service) Process(ctx context.Context, request WagerRequest, key string, trace Trace, message *IncomingMessage) (WagerResult, error) {
	started := time.Now()
	id, err := identifier.New()
	if err != nil {
		return WagerResult{}, err
	}
	input, err := request.Input(id, key)
	if err != nil {
		return WagerResult{}, err
	}
	if _, err := domain.NewWagerTransaction(input, time.Now().UTC()); err != nil {
		return WagerResult{}, err
	}
	var result WagerResult
	err = s.uow.Within(ctx, func(store Store) error {
		if message != nil {
			if err := store.ReserveInbox(ctx, *message); err != nil {
				return err
			}
		}
		transaction, err := store.Identity(ctx, input.ProviderID, input.IdempotencyKey, input.ExternalTransactionID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if transaction != nil {
			result, err = replay(transaction, input)
		} else {
			// O lock precede a inserção: as FKs da transação não devem disputar um upgrade de lock entre escritores.
			wallet, at, lockErr := store.WalletForUpdate(ctx, input.WalletID)
			if lockErr != nil {
				return lockErr
			}
			if wallet.Snapshot().PlayerID != input.PlayerID || wallet.Snapshot().Balance.Currency() != input.Money.Currency() {
				return ErrWalletMismatch
			}
			transaction, err = domain.NewWagerTransaction(input, at)
			if err != nil {
				return err
			}
			reserved, reserveErr := store.ReserveTransaction(ctx, transaction, trace)
			if reserveErr != nil {
				return reserveErr
			}
			if !reserved {
				transaction, err = store.Identity(ctx, input.ProviderID, input.IdempotencyKey, input.ExternalTransactionID)
				if err != nil {
					return err
				}
				result, err = replay(transaction, input)
			} else {
				err = s.apply(ctx, store, transaction, wallet, trace, at, nil)
				if err == nil {
					result = resultOf(transaction, false)
				}
			}
		}
		if err != nil {
			return err
		}
		if message != nil {
			return store.CompleteInbox(ctx, *message, result.TransactionID)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflict) {
			s.observer.Conflict()
		}
		return WagerResult{}, err
	}
	if result.IdempotentReplay {
		s.observer.Replay()
	} else {
		s.observer.Processed(string(request.Kind), string(result.Status), time.Since(started))
	}
	s.faults.Hit("after_financial_commit", request.ExternalTransactionID)
	return result, nil
}

func replay(transaction *domain.WagerTransaction, input domain.TransactionInput) (WagerResult, error) {
	original := transaction.Snapshot().Input
	if original.IdempotencyKey != input.IdempotencyKey || original.ExternalTransactionID != input.ExternalTransactionID || original.PayloadHash != input.PayloadHash {
		return WagerResult{}, ErrConflict
	}
	return resultOf(transaction, true), nil
}

func (s *Service) apply(ctx context.Context, store Store, transaction *domain.WagerTransaction, wallet *domain.Wallet, trace Trace, at time.Time, schedule *ReferenceSchedule) error {
	input := transaction.Snapshot().Input
	before := wallet.Snapshot().Balance
	direction := domain.Direction("")
	resolved := ""
	if input.ReferenceExternalTransactionID != "" {
		reference, err := store.ExternalTransaction(ctx, input.ProviderID, input.ReferenceExternalTransactionID)
		if errors.Is(err, ErrNotFound) {
			return s.waitReference(ctx, store, transaction, wallet, trace, at, schedule)
		}
		if err != nil {
			return err
		}
		ref := reference.Snapshot()
		if ref.Input.WalletID != input.WalletID || ref.Input.PlayerID != input.PlayerID || ref.Input.Money.Currency() != input.Money.Currency() || ref.Input.RoundID != input.RoundID {
			return s.reject(ctx, store, transaction, domain.ReferenceMismatch, before, trace, at)
		}
		if ref.Status == domain.Pending || ref.Status == domain.PendingReference {
			return s.waitReference(ctx, store, transaction, wallet, trace, at, schedule)
		}
		if ref.Status != domain.Processed {
			return s.reject(ctx, store, transaction, domain.ReferenceNotProcessed, before, trace, at)
		}
		validKind := (input.Kind == domain.Win || input.Kind == domain.Refund) && ref.Input.Kind == domain.Bet
		if input.Kind == domain.Rollback {
			validKind = ref.Input.Kind == domain.Bet || ref.Input.Kind == domain.Win || ref.Input.Kind == domain.Refund
		}
		if !validKind {
			return s.reject(ctx, store, transaction, domain.ReferenceMismatch, before, trace, at)
		}
		if input.Kind == domain.Refund || input.Kind == domain.Rollback {
			if ref.Input.Money.MinorUnits() != input.Money.MinorUnits() {
				return s.reject(ctx, store, transaction, domain.AmountMismatch, before, trace, at)
			}
			reversed, err := store.Reversed(ctx, ref.Input.ID)
			if err != nil {
				return err
			}
			if reversed {
				return s.reject(ctx, store, transaction, domain.AlreadyReversed, before, trace, at)
			}
		}
		resolved = ref.Input.ID
		if input.Kind == domain.Rollback && ref.Input.Kind != domain.Bet {
			direction = domain.Debit
		}
	}
	switch input.Kind {
	case domain.Bet:
		direction = domain.Debit
	case domain.Win, domain.Refund:
		direction = domain.Credit
	case domain.Rollback:
		if direction == "" {
			direction = domain.Credit
		}
	}
	var err error
	if direction == domain.Debit {
		err = wallet.Debit(input.Money, at)
	}
	if direction == domain.Credit {
		err = wallet.Credit(input.Money, at)
	}
	if err != nil {
		var code domain.FailureCode
		switch {
		case errors.Is(err, domain.ErrInsufficientFunds):
			code = domain.InsufficientFunds
			if input.Kind == domain.Rollback {
				code = domain.ReversalInsufficientFunds
			}
		case errors.Is(err, domain.ErrMoneyOverflow):
			code = domain.BalanceOverflow
		case errors.Is(err, domain.ErrVersionOverflow):
			code = domain.WalletVersionExhausted
		default:
			return err
		}
		return s.reject(ctx, store, transaction, code, before, trace, at)
	}
	if err := transaction.Process(wallet.Snapshot().Balance, resolved, at); err != nil {
		return err
	}
	if err := store.SaveTransaction(ctx, transaction, nil); err != nil {
		return err
	}
	if direction != "" {
		if err := store.SaveWallet(ctx, wallet); err != nil {
			return err
		}
		return s.recordMovement(ctx, store, transaction, wallet, direction, before, trace, at)
	}
	return s.recordTransaction(ctx, store, transaction, trace)
}

func (s *Service) recordMovement(ctx context.Context, store Store, transaction *domain.WagerTransaction, wallet *domain.Wallet, direction domain.Direction, before domain.Money, trace Trace, at time.Time) error {
	id, err := identifier.New()
	if err != nil {
		return err
	}
	w := wallet.Snapshot()
	entry, err := domain.NewWalletLedgerEntry(domain.LedgerSnapshot{ID: id, WalletID: w.ID, TransactionID: transaction.Snapshot().Input.ID, Direction: direction, Money: transaction.Snapshot().Input.Money, BalanceBefore: before, BalanceAfter: w.Balance, WalletVersion: w.Version, CreatedAt: at})
	if err != nil {
		return err
	}
	if err := store.AddLedger(ctx, entry); err != nil {
		return err
	}
	if err := s.recordTransaction(ctx, store, transaction, trace); err != nil {
		return err
	}
	id, err = identifier.New()
	if err != nil {
		return err
	}
	event, err := domain.NewWalletBalanceChanged(id, trace.CorrelationID, trace.CausationID, entry)
	if err != nil {
		return err
	}
	return store.AddEvents(ctx, []domain.Event{event})
}

func (s *Service) recordTransaction(ctx context.Context, store Store, transaction *domain.WagerTransaction, trace Trace) error {
	id, err := identifier.New()
	if err != nil {
		return err
	}
	var event domain.Event
	switch transaction.Snapshot().Status {
	case domain.Processed:
		event, err = domain.NewWagerTransactionProcessed(id, trace.CorrelationID, trace.CausationID, transaction)
	case domain.Rejected:
		event, err = domain.NewWagerTransactionRejected(id, trace.CorrelationID, trace.CausationID, transaction)
	case domain.PendingReference:
		event, err = domain.NewWagerTransactionPendingReference(id, trace.CorrelationID, trace.CausationID, transaction)
	default:
		return domain.ErrInvalidTransition
	}
	if err != nil {
		return err
	}
	return store.AddEvents(ctx, []domain.Event{event})
}

func (s *Service) reject(ctx context.Context, store Store, transaction *domain.WagerTransaction, code domain.FailureCode, balance domain.Money, trace Trace, at time.Time) error {
	if err := transaction.Reject(code, balance, at); err != nil {
		return err
	}
	if err := store.SaveTransaction(ctx, transaction, nil); err != nil {
		return err
	}
	return s.recordTransaction(ctx, store, transaction, trace)
}

func (s *Service) waitReference(ctx context.Context, store Store, transaction *domain.WagerTransaction, wallet *domain.Wallet, trace Trace, at time.Time, schedule *ReferenceSchedule) error {
	first := transaction.Snapshot().Status != domain.PendingReference
	next := ReferenceSchedule{Expires: at.Add(s.policy.TTL)}
	if schedule != nil {
		next = *schedule
	}
	next.Attempts++
	if !at.Before(next.Expires) || next.Attempts > s.policy.MaxAttempts {
		return s.reject(ctx, store, transaction, domain.ReferenceNotFound, wallet.Snapshot().Balance, trace, at)
	}
	delay := s.policy.RetryBase
	for i := 1; i < next.Attempts && delay < 30*time.Second; i++ {
		delay *= 2
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	next.Next = at.Add(delay)
	if next.Next.After(next.Expires) {
		next.Next = next.Expires
	}
	next.LeaseID = ""
	if err := transaction.AwaitReference(at); err != nil {
		return err
	}
	if err := store.SaveTransaction(ctx, transaction, &next); err != nil {
		return err
	}
	if first {
		return s.recordTransaction(ctx, store, transaction, trace)
	}
	return nil
}

func (s *Service) Resume(ctx context.Context, transactionID, leaseID string) error {
	started := time.Now()
	var completed *domain.WagerTransaction
	err := s.uow.Within(ctx, func(store Store) error {
		transaction, err := store.Transaction(ctx, transactionID)
		if err != nil {
			return err
		}
		if transaction.Snapshot().Status != domain.PendingReference {
			return nil
		}
		wallet, at, err := store.WalletForUpdate(ctx, transaction.Snapshot().Input.WalletID)
		if err != nil {
			return err
		}
		transaction, err = store.Transaction(ctx, transactionID)
		if err != nil {
			return err
		}
		if transaction.Snapshot().Status != domain.PendingReference {
			return nil
		}
		schedule, err := store.ReferenceSchedule(ctx, transactionID)
		if err != nil {
			return err
		}
		if leaseID == "" || schedule.LeaseID != leaseID || !at.Before(schedule.LeaseUntil) {
			return ErrLeaseLost
		}
		trace, err := store.TransactionTrace(ctx, transactionID)
		if err != nil {
			return err
		}
		if !at.Before(schedule.Expires) || schedule.Attempts >= s.policy.MaxAttempts {
			err = s.reject(ctx, store, transaction, domain.ReferenceNotFound, wallet.Snapshot().Balance, trace, at)
		} else {
			err = s.apply(ctx, store, transaction, wallet, trace, at, &schedule)
		}
		if err == nil && transaction.Snapshot().Status != domain.PendingReference {
			completed = transaction
		}
		return err
	})
	if err == nil && completed != nil {
		s.observer.Processed(string(completed.Snapshot().Input.Kind), string(completed.Snapshot().Status), time.Since(started))
	}
	return err
}
