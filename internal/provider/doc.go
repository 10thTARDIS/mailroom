// SPDX-License-Identifier: AGPL-3.0-only

// Package provider holds provider-profile capability detection (spec §16
// provider profiles, §18.1). Rather than file a support ticket to learn
// whether a mail provider supports SMTPUTF8/EAI (RFC 6531), we probe the
// SMTP server's advertised ESMTP extensions directly and record the answer
// in the provider profile. The concurrent-IMAP connection cap is handled
// separately by keeping the sync pool conservative by default (one long-lived
// IDLE connection plus a small worker pool), so it is never a blocker.
package provider
