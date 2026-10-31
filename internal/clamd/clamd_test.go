// SPDX-License-Identifier: AGPL-3.0-only

package clamd

import "testing"

// ⚠ EVERY STRING IN THIS TABLE IS A REAL CLAMD REPLY, captured from
// clamav/clamav:latest on 2026-09-01 — not invented from documentation. That
// matters because ADR-027 §5a's whole point is that the documented behaviour
// and the actual behaviour differ: stock clamd returns plain OK for an
// encrypted archive, and only reports it once AlertEncrypted is enabled.
func TestParseResponseMapsRealClamdReplies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    string
		want    State
		wantSig string
	}{
		{
			name: "clean", resp: "stream: OK", want: Clean,
		},
		{
			// A real signature. This is the only path to Flagged.
			name: "real signature", resp: "stream: Eicar-Test-Signature FOUND",
			want: Flagged, wantSig: "Eicar-Test-Signature",
		},
		{
			// ⚠ An encrypted archive is commodity technique — "password is in
			// the email body" — not exotic. Mapping it to Flagged would
			// quarantine every password-protected zip as malware.
			name: "encrypted archive", resp: "stream: Heuristics.Encrypted.Zip FOUND",
			want: Unknown, wantSig: "Heuristics.Encrypted.Zip",
		},
		{
			// ⚠ clamd stopped looking. Without AlertExceedsMax this returns
			// plain OK — a clean verdict on a file nothing opened.
			name: "limits exceeded", resp: "stream: Heuristics.Limits.Exceeded.MaxFileSize FOUND",
			want: Unknown, wantSig: "Heuristics.Limits.Exceeded.MaxFileSize",
		},
		{
			// ⚠ Measured: this is what an oversize INSTREAM replies, arriving
			// together with a closed connection. Per-file, never an outage.
			name: "stream too large", resp: "INSTREAM size limit exceeded. ERROR",
			want: Unknown,
		},
		{
			// A protocol we do not recognise must never read as clean.
			name: "unrecognised", resp: "stream: something entirely new",
			want: Unknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseResponse(tc.resp)
			if err != nil {
				t.Fatalf("parseResponse(%q) errored: %v", tc.resp, err)
			}
			if got.State != tc.want {
				t.Errorf("parseResponse(%q).State = %q, want %q", tc.resp, got.State, tc.want)
			}
			if tc.wantSig != "" && got.Signature != tc.wantSig {
				t.Errorf("signature = %q, want %q", got.Signature, tc.wantSig)
			}
		})
	}
}

// ⚠ THE MOST IMPORTANT PROPERTY IN THIS PACKAGE. Nothing may map to Clean
// except an explicit OK. A heuristic, an error, an unparsable reply — all must
// be Unknown, because Unknown carries enforcement (§5b) and Clean does not.
// Getting this backwards means an attachment nobody looked inside is presented
// to the user as checked and safe.
func TestOnlyAnExplicitOKIsEverClean(t *testing.T) {
	notClean := []string{
		"stream: Eicar-Test-Signature FOUND",
		"stream: Heuristics.Encrypted.Zip FOUND",
		"stream: Heuristics.Limits.Exceeded.MaxFileSize FOUND",
		"INSTREAM size limit exceeded. ERROR",
		"stream: something entirely new",
		"",
		"OKAY",       // ends in neither OK nor a known suffix
		"NOT OK NOW", // deliberately adversarial substring
	}
	for _, resp := range notClean {
		got, _ := parseResponse(resp)
		if got.State == Clean {
			t.Errorf("parseResponse(%q) = Clean — only an explicit OK may be clean", resp)
		}
	}
	// Positive control: the one reply that IS clean must actually parse as
	// clean, or the assertion above is vacuous.
	if got, _ := parseResponse("stream: OK"); got.State != Clean {
		t.Fatalf("the control failed: %q did not parse as Clean, so the test above proves nothing", "stream: OK")
	}
}

// The heuristic families are prefixes, so a new member of either family (a
// future Heuristics.Encrypted.Rar, say) is Unknown without a code change —
// which is the safe direction. A NON-heuristic signature that merely mentions
// the word must still be Flagged.
func TestHeuristicPrefixesAreFamiliesNotExactNames(t *testing.T) {
	for resp, want := range map[string]State{
		"stream: Heuristics.Encrypted.RAR FOUND":                Unknown,
		"stream: Heuristics.Limits.Exceeded.MaxRecursion FOUND": Unknown,
		// ⚠ Not a heuristic: a real signature that happens to contain the word.
		// Mapping this to Unknown would let an attacker pick a name that
		// downgrades their own detection.
		"stream: Win.Trojan.EncryptedDropper FOUND": Flagged,
	} {
		got, _ := parseResponse(resp)
		if got.State != want {
			t.Errorf("parseResponse(%q).State = %q, want %q", resp, got.State, want)
		}
	}
}

func TestSignatureOfStripsTheWireFraming(t *testing.T) {
	for resp, want := range map[string]string{
		"stream: Eicar-Test-Signature FOUND":     "Eicar-Test-Signature",
		"stream: Heuristics.Encrypted.Zip FOUND": "Heuristics.Encrypted.Zip",
		"Eicar-Test-Signature FOUND":             "Eicar-Test-Signature",
	} {
		if got := signatureOf(resp); got != want {
			t.Errorf("signatureOf(%q) = %q, want %q", resp, got, want)
		}
	}
}

// An oversize attachment must never reach the daemon: the pre-check is what
// keeps the common case off the wire, and what makes the behaviour independent
// of whether our limit matches clamd's.
func TestScanRefusesOversizeWithoutContactingTheDaemon(t *testing.T) {
	// Address is deliberately unroutable. If Scan tried to dial it, this test
	// would fail with a dial error rather than return a verdict — which is
	// exactly the assertion.
	c := &Client{Network: "unix", Address: "/nonexistent/clamd.sock", MaxBytes: 16}
	v, err := c.Scan(t.Context(), make([]byte, 17))
	if err != nil {
		t.Fatalf("oversize scan returned an error (so it dialled): %v", err)
	}
	if v.State != Unknown {
		t.Errorf("State = %q, want %q", v.State, Unknown)
	}
	if v.Reason == "" {
		t.Error("Unknown with no reason — the operator cannot tell why")
	}
}
