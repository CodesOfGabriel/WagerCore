package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrInvalidAmount    = errors.New("invalid monetary amount")
	ErrInvalidCurrency  = errors.New("unsupported currency")
	ErrCurrencyMismatch = errors.New("currencies do not match")
	ErrMoneyOverflow    = errors.New("monetary amount exceeds int64 limits")
)

type Currency string

const (
	BRL Currency = "BRL"
	USD Currency = "USD"
	EUR Currency = "EUR"
)

func (c Currency) Validate() error {
	switch c {
	case BRL, USD, EUR:
		return nil
	default:
		return ErrInvalidCurrency
	}
}

type Money struct {
	minorUnits int64
	currency   Currency
}

func NewMoney(amount string, currency Currency) (Money, error) {
	if err := currency.Validate(); err != nil {
		return Money{}, err
	}
	whole, fraction, found := strings.Cut(amount, ".")
	if !found || len(whole) == 0 || len(fraction) != 2 || (len(whole) > 1 && whole[0] == '0') {
		return Money{}, ErrInvalidAmount
	}
	for _, part := range []string{whole, fraction} {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return Money{}, ErrInvalidAmount
			}
		}
	}
	units, err := strconv.ParseInt(whole+fraction, 10, 64)
	if err != nil {
		return Money{}, ErrMoneyOverflow
	}
	return Money{minorUnits: units, currency: currency}, nil
}

func MoneyFromMinorUnits(units int64, currency Currency) (Money, error) {
	if err := currency.Validate(); err != nil {
		return Money{}, err
	}
	return Money{minorUnits: units, currency: currency}, nil
}

func ZeroMoney(currency Currency) (Money, error) {
	return MoneyFromMinorUnits(0, currency)
}

func (m Money) Validate() error {
	return m.currency.Validate()
}

func (m Money) Currency() Currency {
	return m.currency
}

func (m Money) MinorUnits() int64 {
	return m.minorUnits
}

func (m Money) Amount() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	whole, fraction := m.minorUnits/100, m.minorUnits%100
	if fraction < 0 {
		fraction = -fraction
	}
	if m.minorUnits < 0 && whole == 0 {
		return fmt.Sprintf("-0.%02d", fraction), nil
	}
	return fmt.Sprintf("%d.%02d", whole, fraction), nil
}

func (m Money) compatible(other Money) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := other.Validate(); err != nil {
		return err
	}
	if m.currency != other.currency {
		return ErrCurrencyMismatch
	}
	return nil
}

func (m Money) Add(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	a, b := m.minorUnits, other.minorUnits
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{minorUnits: a + b, currency: m.currency}, nil
}

func (m Money) Subtract(other Money) (Money, error) {
	if err := m.compatible(other); err != nil {
		return Money{}, err
	}
	a, b := m.minorUnits, other.minorUnits
	if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{minorUnits: a - b, currency: m.currency}, nil
}

func (m Money) Negate() (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	if m.minorUnits == math.MinInt64 {
		return Money{}, ErrMoneyOverflow
	}
	return Money{minorUnits: -m.minorUnits, currency: m.currency}, nil
}

func (m Money) Compare(other Money) (int, error) {
	if err := m.compatible(other); err != nil {
		return 0, err
	}
	switch {
	case m.minorUnits < other.minorUnits:
		return -1, nil
	case m.minorUnits > other.minorUnits:
		return 1, nil
	default:
		return 0, nil
	}
}

func (m Money) MarshalJSON() ([]byte, error) {
	amount, err := m.Amount()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Amount   string   `json:"amount"`
		Currency Currency `json:"currency"`
	}{Amount: amount, Currency: m.currency})
}

func (m *Money) UnmarshalJSON(data []byte) error {
	var value struct {
		Amount   string   `json:"amount"`
		Currency Currency `json:"currency"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode money: %w", err)
	}
	parsed, err := NewMoney(value.Amount, value.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
