// SPDX-License-Identifier: AGPL-3.0-only

// Package dkimtest signs messages for tests of code that uses dkimverify, so
// those tests need neither the network nor a copy of the DKIM library. Test
// support only: nothing outside _test.go files imports it.
package dkimtest

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

// Key is a throwaway ed25519 signing key.
type Key struct {
	pub ed25519.PublicKey
	prv ed25519.PrivateKey
}

// NewKey generates a key.
func NewKey(t testing.TB) *Key {
	t.Helper()
	pub, prv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{pub: pub, prv: prv}
}

// Signer returns the private key, for tests that need custom SignOptions.
func (k *Key) Signer() crypto.Signer { return k.prv }

// Record is the TXT record that publishes the key.
func (k *Key) Record() string {
	return "v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(k.pub)
}

// Sign returns msg (CRLF header block, blank line, body) with a DKIM-Signature
// for domain/selector prepended. All headers present are signed.
func (k *Key) Sign(t testing.TB, msg, domain, selector string) string {
	t.Helper()
	var out bytes.Buffer
	err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{Domain: domain, Selector: selector, Signer: k.prv})
	if err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// Name is the DNS name a signature for domain/selector queries.
func Name(domain, selector string) string { return selector + "._domainkey." + domain }
