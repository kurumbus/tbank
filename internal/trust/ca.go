package trust

import (
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
)

// Russian Trusted Root CA, downloaded from the Ministry of Digital Development
// distribution at https://gu-st.ru/content/lending/russian_trusted_root_ca_pem.crt
//
//go:embed russian_trusted_root_ca.pem
var rootPEM []byte

// rootSHA256 is the published fingerprint of Russian Trusted Root CA.
const rootSHA256 = "d26d2d0231b7c39f92cc738512ba54103519e4405d68b5bd703e9788ca8ecf31"

// CAFile returns a PEM path the invest-go client can use as its only trust anchor.
// A non-empty override is used as-is. Otherwise the bundled root is written to a
// temp file; cleanup removes that file.
func CAFile(override string) (string, func(), error) {
	if override != "" {
		return override, func() {}, nil
	}
	if err := CheckRoot(); err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp("", "tbank-russian-trusted-root-*.pem")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(rootPEM); err != nil {
		name := f.Name()
		f.Close()
		os.Remove(name)
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	name := f.Name()
	return name, func() { os.Remove(name) }, nil
}

// CheckRoot parses the bundled root and rejects a file whose fingerprint changed.
func CheckRoot() error {
	block, _ := pem.Decode(rootPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return fmt.Errorf("bundled Russian Trusted Root CA is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse bundled Russian Trusted Root CA: %w", err)
	}
	sum := sha256.Sum256(cert.Raw)
	got := hex.EncodeToString(sum[:])
	if got != rootSHA256 {
		return fmt.Errorf("bundled Russian Trusted Root CA fingerprint is %s", got)
	}
	return nil
}
