package domain_test

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"testing"

	"junglegaming/internal/domain"
)

func TestNewMoney(t *testing.T) {
	tests := []struct {
		amount string
		units  int64
	}{
		{"0.00", 0},
		{"0.01", 1},
		{"25.00", 2500},
		{"1000.50", 100050},
		{"92233720368547758.07", math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.amount, func(t *testing.T) {
			money, err := domain.NewMoney(tt.amount, domain.BRL)
			if err != nil {
				t.Fatal(err)
			}
			amount, err := money.Amount()
			if err != nil || amount != tt.amount || money.MinorUnits() != tt.units || money.Currency() != domain.BRL {
				t.Fatalf("got amount=%q units=%d currency=%s error=%v", amount, money.MinorUnits(), money.Currency(), err)
			}
		})
	}
}

func TestNewMoneyRejectsInvalidAmounts(t *testing.T) {
	for _, amount := range []string{
		"", "NaN", "Infinity", "1e3", "1E3", "1", "1.0", "1.000", ".00", "1.",
		"-1.00", "-0.00", "+1.00", "01.00", "00.00", " 1.00", "1.00 ",
		"1,00", "1..00", "１.00", "1.0a", "a.00", "1.0\n",
	} {
		t.Run(amount, func(t *testing.T) {
			if _, err := domain.NewMoney(amount, domain.BRL); !errors.Is(err, domain.ErrInvalidAmount) {
				t.Fatalf("got %v, want ErrInvalidAmount", err)
			}
		})
	}
	for _, amount := range []string{"92233720368547758.08", "999999999999999999999999999999.00"} {
		if _, err := domain.NewMoney(amount, domain.BRL); !errors.Is(err, domain.ErrMoneyOverflow) {
			t.Fatalf("amount=%s error=%v, want ErrMoneyOverflow", amount, err)
		}
	}
}

func TestMoneyCurrencyValidation(t *testing.T) {
	for _, currency := range []domain.Currency{"", "brl", "ZZZ", "BR", "BRLL"} {
		if _, err := domain.NewMoney("1.00", currency); !errors.Is(err, domain.ErrInvalidCurrency) {
			t.Fatalf("currency=%q error=%v", currency, err)
		}
		if _, err := domain.MoneyFromMinorUnits(1, currency); !errors.Is(err, domain.ErrInvalidCurrency) {
			t.Fatalf("currency=%q error=%v", currency, err)
		}
	}
	for _, currency := range []domain.Currency{domain.BRL, domain.USD, domain.EUR} {
		zero, err := domain.ZeroMoney(currency)
		if err != nil || zero.MinorUnits() != 0 || zero.Currency() != currency {
			t.Fatalf("currency=%q money=%v error=%v", currency, zero, err)
		}
	}
}

func TestMoneyArithmetic(t *testing.T) {
	left := fromUnits(t, 10000, domain.BRL)
	right := fromUnits(t, 2500, domain.BRL)
	assertUnits := func(result domain.Money, err error, want int64) {
		t.Helper()
		if err != nil || result.MinorUnits() != want || result.Currency() != domain.BRL {
			t.Fatalf("got %v, error=%v, want %d BRL", result, err, want)
		}
	}
	result, err := left.Add(right)
	assertUnits(result, err, 12500)
	result, err = left.Subtract(right)
	assertUnits(result, err, 7500)
	result, err = right.Subtract(left)
	assertUnits(result, err, -7500)
	result, err = right.Negate()
	assertUnits(result, err, -2500)
	if left.MinorUnits() != 10000 || right.MinorUnits() != 2500 {
		t.Fatal("arithmetic changed an operand")
	}
	for _, tt := range []struct {
		left, right int64
		want        int
	}{
		{1, 2, -1}, {2, 2, 0}, {3, 2, 1}, {math.MinInt64, math.MaxInt64, -1},
	} {
		got, err := fromUnits(t, tt.left, domain.BRL).Compare(fromUnits(t, tt.right, domain.BRL))
		if err != nil || got != tt.want {
			t.Fatalf("compare(%d, %d)=%d error=%v", tt.left, tt.right, got, err)
		}
	}
}

func TestMoneyRejectsIncompatibleOrUninitializedValues(t *testing.T) {
	valid := fromUnits(t, 100, domain.BRL)
	for _, tt := range []struct {
		name  string
		left  domain.Money
		right domain.Money
		want  error
	}{
		{"currency mismatch", valid, fromUnits(t, 100, domain.USD), domain.ErrCurrencyMismatch},
		{"uninitialized left", domain.Money{}, valid, domain.ErrInvalidCurrency},
		{"uninitialized right", valid, domain.Money{}, domain.ErrInvalidCurrency},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, addErr := tt.left.Add(tt.right)
			_, subErr := tt.left.Subtract(tt.right)
			_, compareErr := tt.left.Compare(tt.right)
			for _, err := range []error{addErr, subErr, compareErr} {
				if !errors.Is(err, tt.want) {
					t.Fatalf("got %v, want %v", err, tt.want)
				}
			}
		})
	}
	var zero domain.Money
	_, amountErr := zero.Amount()
	_, negateErr := zero.Negate()
	_, jsonErr := json.Marshal(zero)
	for _, err := range []error{zero.Validate(), amountErr, negateErr, jsonErr} {
		if !errors.Is(err, domain.ErrInvalidCurrency) {
			t.Fatalf("uninitialized money error=%v", err)
		}
	}
}

func TestMoneyLimits(t *testing.T) {
	for _, tt := range []struct {
		units  int64
		amount string
	}{
		{math.MinInt64, "-92233720368547758.08"},
		{math.MaxInt64, "92233720368547758.07"},
		{-1, "-0.01"}, {-99, "-0.99"}, {-100, "-1.00"}, {-101, "-1.01"},
	} {
		amount, err := fromUnits(t, tt.units, domain.BRL).Amount()
		if err != nil || amount != tt.amount {
			t.Fatalf("units=%d amount=%q error=%v", tt.units, amount, err)
		}
	}
	if _, err := fromUnits(t, math.MinInt64, domain.BRL).Negate(); !errors.Is(err, domain.ErrMoneyOverflow) {
		t.Fatalf("negation error=%v", err)
	}
	negated, err := fromUnits(t, math.MaxInt64, domain.BRL).Negate()
	if err != nil || negated.MinorUnits() != -math.MaxInt64 {
		t.Fatalf("negation result=%v error=%v", negated, err)
	}
}

func TestMoneyJSON(t *testing.T) {
	money := fromUnits(t, 2500, domain.BRL)
	encoded, err := json.Marshal(money)
	if err != nil || string(encoded) != `{"amount":"25.00","currency":"BRL"}` {
		t.Fatalf("encoded=%s error=%v", encoded, err)
	}
	var decoded domain.Money
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != money {
		t.Fatalf("decoded=%v error=%v", decoded, err)
	}
	for _, input := range []string{
		`{"amount":25.00,"currency":"BRL"}`, `{"amount":"-1.00","currency":"BRL"}`,
		`{"amount":"1.00"}`, `{"currency":"BRL"}`, `null`, `{}`, `[]`,
		`{"amount":"1.00","currency":"ZZZ"}`, `{"amount":null,"currency":"BRL"}`,
		`{"amount":"1.00","currency":null}`, `{"amount":`,
	} {
		decoded := money
		if err := json.Unmarshal([]byte(input), &decoded); err == nil {
			t.Fatalf("accepted %s", input)
		}
		if decoded != money {
			t.Fatalf("invalid input changed receiver: %s", input)
		}
	}
	negative, err := json.Marshal(fromUnits(t, -1, domain.BRL))
	if err != nil || string(negative) != `{"amount":"-0.01","currency":"BRL"}` {
		t.Fatalf("encoded=%s error=%v", negative, err)
	}
}

func FuzzMoneyArithmetic(f *testing.F) {
	for _, pair := range [][2]int64{
		{0, 0}, {10000, 2500}, {math.MaxInt64, 1}, {math.MinInt64, -1},
		{math.MinInt64, 1}, {math.MaxInt64, -1}, {0, math.MinInt64},
		{math.MinInt64, math.MaxInt64}, {math.MinInt64, math.MinInt64},
	} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, a, b int64) {
		left := fromUnits(t, a, domain.BRL)
		right := fromUnits(t, b, domain.BRL)
		for _, op := range []struct {
			apply func(domain.Money) (domain.Money, error)
			want  *big.Int
		}{
			{left.Add, new(big.Int).Add(big.NewInt(a), big.NewInt(b))},
			{left.Subtract, new(big.Int).Sub(big.NewInt(a), big.NewInt(b))},
		} {
			got, err := op.apply(right)
			if !op.want.IsInt64() {
				if !errors.Is(err, domain.ErrMoneyOverflow) {
					t.Fatalf("a=%d b=%d result=%s error=%v", a, b, op.want, err)
				}
				continue
			}
			if err != nil || got.MinorUnits() != op.want.Int64() {
				t.Fatalf("a=%d b=%d got=%d want=%s error=%v", a, b, got.MinorUnits(), op.want, err)
			}
		}
	})
}

func FuzzMoneyParsing(f *testing.F) {
	for _, amount := range []string{"0.00", "25.00", "92233720368547758.07", "92233720368547758.08", "-1.00", "NaN", ""} {
		f.Add(amount)
	}
	f.Fuzz(func(t *testing.T, input string) {
		money, err := domain.NewMoney(input, domain.BRL)
		if err != nil {
			return
		}
		amount, err := money.Amount()
		if err != nil || amount != input || money.MinorUnits() < 0 {
			t.Fatalf("input=%q amount=%q units=%d error=%v", input, amount, money.MinorUnits(), err)
		}
		encoded, err := json.Marshal(money)
		if err != nil {
			t.Fatal(err)
		}
		var decoded domain.Money
		if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != money {
			t.Fatalf("round trip: got=%v want=%v error=%v", decoded, money, err)
		}
	})
}

func fromUnits(t *testing.T, units int64, currency domain.Currency) domain.Money {
	t.Helper()
	money, err := domain.MoneyFromMinorUnits(units, currency)
	if err != nil {
		t.Fatal(err)
	}
	return money
}
