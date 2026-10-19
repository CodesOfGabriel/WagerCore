package domain

import "errors"

var (
	ErrInvalidID           = errors.New("invalid identifier")
	ErrInvalidTimestamp    = errors.New("invalid timestamp")
	ErrInvalidWallet       = errors.New("invalid wallet")
	ErrInvalidVersion      = errors.New("invalid wallet version")
	ErrVersionOverflow     = errors.New("wallet version exceeds int64 limits")
	ErrInsufficientFunds   = errors.New("insufficient funds")
	ErrInvalidTransaction  = errors.New("invalid wager transaction")
	ErrInvalidTransition   = errors.New("invalid transaction transition")
	ErrTerminalTransaction = errors.New("transaction is already terminal")
	ErrInvalidReference    = errors.New("invalid transaction reference")
	ErrInvalidLedgerEntry  = errors.New("invalid ledger entry")
)
