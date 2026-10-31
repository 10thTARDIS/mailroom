// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"testing"

	"mailroom/internal/config"
	"mailroom/internal/provider"
)

// TestSubmissionSenderHonorsAllowPrivateAccountHosts: the send worker's SMTP
// dial is guarded (#625 §3), so MAILROOM_ALLOW_PRIVATE_ACCOUNT_HOSTS must reach
// it — otherwise an operator whose mail server is on the LAN could add the
// account (validation honours the switch) and then never send from it. And
// without the switch the sender must be the guarded one.
func TestSubmissionSenderHonorsAllowPrivateAccountHosts(t *testing.T) {
	for _, allow := range []bool{false, true} {
		s, ok := submissionSender(&config.Config{AllowPrivateAccountHosts: allow}).(provider.SMTPSender)
		if !ok {
			t.Fatalf("submissionSender is not a provider.SMTPSender")
		}
		if s.AllowPrivateHosts != allow {
			t.Errorf("AllowPrivateAccountHosts=%v reached the submission dial as %v", allow, s.AllowPrivateHosts)
		}
	}
}
