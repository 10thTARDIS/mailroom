// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"errors"
	"fmt"

	"mailroom/internal/provider"
	"mailroom/internal/store"
	"mailroom/internal/sync"
)

// explainIMAPAccess puts the profile's plain-words hint in front of an IMAP
// refusal of a freshly issued OAuth token (sync.ErrIMAPAccessRefused,
// ADR-046 §12), so the account's recorded last error — what
// Account/getHealth's syncStatus.lastError shows — says "IMAP access is off
// for this mailbox; turn it on in Outlook.com settings" rather than
// "XOAUTH2 refused: NO AUTHENTICATE failed.". The hint text is stable (it
// leads the message); the original error stays wrapped for errors.Is/As.
// Any other error is returned unchanged.
func explainIMAPAccess(acct store.Account, err error) error {
	if err == nil || !errors.Is(err, sync.ErrIMAPAccessRefused) {
		return err
	}
	return fmt.Errorf("%s: %w", provider.IMAPAccessHint(acct.ProviderProfile), err)
}
