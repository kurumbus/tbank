package client

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"google.golang.org/grpc"
	"opensource.tbank.ru/invest/invest-go/investgo"

	"tbank/internal/config"
	"tbank/internal/trust"
)

// Dial opens a gRPC client. When AccountID is empty, the SDK selects an open
// sandbox account or creates one. The returned account id is the one it chose.
// TLS trusts the bundled Russian Trusted Root CA unless TBANK_TLS_CA_FILE is set.
func Dial(ctx context.Context, settings config.Settings) (*investgo.Client, error) {
	caFile, cleanup, err := trust.CAFile(settings.CAFile)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// The default 4MB gRPC cap is too small for the full bond and option catalogs.
	c, err := investgo.NewClient(ctx, settings.SDKConfig(caFile), sdkLogger{},
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(128<<20)),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", settings.Endpoint, err)
	}
	return c, nil
}

type sdkLogger struct{}

func (sdkLogger) Infof(template string, args ...any) {
	slog.Info(fmt.Sprintf(template, args...))
}

func (sdkLogger) Errorf(template string, args ...any) {
	slog.Error(fmt.Sprintf(template, args...))
}

func (sdkLogger) Fatalf(template string, args ...any) {
	slog.Error(fmt.Sprintf(template, args...))
	os.Exit(1)
}
