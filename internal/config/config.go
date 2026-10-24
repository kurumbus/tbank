package config

import (
	"fmt"
	"os"
	"strings"

	"opensource.tbank.ru/invest/invest-go/investgo"
)

const (
	defaultEndpoint = "sandbox-invest-public-api.tbank.ru:443"
	defaultAppName  = "tbank"
	envFile         = ".env"
)

// Settings is the process configuration for the T-Bank Invest client.
type Settings struct {
	Token     string
	Endpoint  string
	AccountID string
	AppName   string
	// CAFile, when set, replaces the bundled Russian Trusted Root CA.
	CAFile string
}

// Load reads .env from the working directory, then TBANK_* variables.
func Load() (Settings, error) {
	if err := LoadDotEnv(envFile); err != nil {
		return Settings{}, err
	}

	token := strings.TrimSpace(os.Getenv("TBANK_API_TOKEN"))
	if token == "" {
		return Settings{}, fmt.Errorf("TBANK_API_TOKEN is empty; set it in the environment or .env")
	}

	endpoint := strings.TrimSpace(os.Getenv("TBANK_ENDPOINT"))
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	appName := strings.TrimSpace(os.Getenv("TBANK_APP_NAME"))
	if appName == "" {
		appName = defaultAppName
	}

	return Settings{
		Token:     token,
		Endpoint:  endpoint,
		AccountID: strings.TrimSpace(os.Getenv("TBANK_ACCOUNT_ID")),
		AppName:   appName,
		CAFile:    strings.TrimSpace(os.Getenv("TBANK_TLS_CA_FILE")),
	}, nil
}

// SDKConfig maps settings onto the official invest-go client config.
// An empty AccountID makes the SDK reuse an open sandbox account, or open one.
// ResourceExhausted retry is disabled because the SDK also uses that code for
// oversized replies and aborts when the rate-limit reset header is absent.
func (s Settings) SDKConfig(caFile string) investgo.Config {
	return investgo.Config{
		EndPoint:                      s.Endpoint,
		Token:                         s.Token,
		AppName:                       s.AppName,
		AccountId:                     s.AccountID,
		TLSCACertFile:                 caFile,
		DisableResourceExhaustedRetry: true,
	}
}
