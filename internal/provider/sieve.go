// SPDX-License-Identifier: AGPL-3.0-only

package provider

// ManageSieve endpoints per provider profile (spec §16 provider profiles, #50).
//
// An account row stores its IMAP and SMTP endpoints but no ManageSieve one, and
// the vacation responder must not need a schema change before anyone has seen
// whether the provider answers at all. The convention every mainstream
// ManageSieve deployment follows — the IMAP host, on the IANA port 4190, in
// cleartext upgraded by STARTTLS (RFC 5804 §1.8) — is therefore the default,
// and a profile overrides it only where its provider is known to differ.
//
// ⚠ FOR "migadu" THIS IS THE CONVENTION, NOT A MEASUREMENT. The spec records
// ManageSieve as confirmed available on Migadu; the host and port are what the
// first `mailroom-admin sieve-report` run against a real account confirms or
// refutes, and its --host/--port flags exist so a wrong guess costs a flag, not
// a release.

// SieveEndpoint is where to reach an account's ManageSieve service.
type SieveEndpoint struct {
	Host        string
	Port        int
	ImplicitTLS bool
	// Source says how the endpoint was derived, for the report to print.
	Source string
}

// ManageSievePort is the IANA ManageSieve port (RFC 5804 §1.8).
const ManageSievePort = 4190

// noManageSieve lists profiles whose provider has no ManageSieve service.
var noManageSieve = map[string]string{
	// Gmail filters are configured in its web UI or its own API; there is no
	// ManageSieve server.
	"gmail": "Gmail offers no ManageSieve service",
}

// ManageSieveEndpoint derives the ManageSieve endpoint for an account from its
// provider profile and IMAP host. ok is false (with a reason) when the profile
// is known to have no ManageSieve service.
func ManageSieveEndpoint(profile, imapHost string) (ep SieveEndpoint, ok bool, reason string) {
	if why, none := noManageSieve[profile]; none {
		return SieveEndpoint{}, false, why
	}
	return SieveEndpoint{
		Host:   imapHost,
		Port:   ManageSievePort,
		Source: "the account's IMAP host on the RFC 5804 port, STARTTLS",
	}, true, ""
}
