package bootstrap

import (
	"testing"

	"go.uber.org/fx"
)

func TestDependencyGraph(t *testing.T) {
	if err := fx.ValidateApp(module(), fx.NopLogger); err != nil {
		t.Fatal(err)
	}
}
