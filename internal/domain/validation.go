package domain

import (
	"encoding/hex"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func ValidateID(id string) error {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id != strings.ToLower(id) {
		return ErrInvalidID
	}
	value, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(value) != 16 {
		return ErrInvalidID
	}
	for _, b := range value {
		if b != 0 {
			return nil
		}
	}
	return ErrInvalidID
}

func validText(value string) bool {
	if value == "" || len(value) > 255 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validTimes(createdAt, updatedAt time.Time) bool {
	return !createdAt.IsZero() && !updatedAt.IsZero() && !updatedAt.Before(createdAt)
}

func validateNonNegative(money Money) error {
	if err := money.Validate(); err != nil {
		return err
	}
	if money.MinorUnits() < 0 {
		return ErrInvalidAmount
	}
	return nil
}

func validatePositive(money Money) error {
	if err := validateNonNegative(money); err != nil {
		return err
	}
	if money.MinorUnits() == 0 {
		return ErrInvalidAmount
	}
	return nil
}
