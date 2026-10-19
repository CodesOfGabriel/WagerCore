package fault

import (
	"log/slog"
	"os"

	"junglegaming/internal/platform/config"
)

type Injector struct {
	cfg    config.Config
	logger *slog.Logger
}

func New(cfg config.Config, logger *slog.Logger) *Injector { return &Injector{cfg, logger} }
func (i *Injector) Hit(point, external string) {
	if i.cfg.Environment == "test" && i.cfg.FaultPoint == point && i.cfg.FaultTransaction != "" && i.cfg.FaultTransaction == external {
		i.logger.Warn("fault_injected", "point", point, "externalTransactionId", external)
		os.Exit(97)
	}
}
