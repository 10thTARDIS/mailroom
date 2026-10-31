// SPDX-License-Identifier: AGPL-3.0-only

package syncrunner

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"mailroom/internal/store"
	"mailroom/internal/sync"
)

func TestExplainIMAPAccess(t *testing.T) {
	refused := &sync.Error{Kind: sync.ErrorKindAuth, Op: "login", Err: fmt.Errorf("x: %w", sync.ErrIMAPAccessRefused)}
	got := explainIMAPAccess(store.Account{ProviderProfile: "microsoft"}, fmt.Errorf("dial: %w", refused))
	if !strings.HasPrefix(got.Error(), "IMAP access is off for this mailbox") ||
		!strings.Contains(got.Error(), "Forwarding and IMAP") {
		t.Errorf("hint = %q", got.Error())
	}
	var se *sync.Error
	if !errors.As(got, &se) || se.Kind != sync.ErrorKindAuth {
		t.Error("the original error is no longer wrapped")
	}
	other := errors.New("dial: connection refused")
	if explainIMAPAccess(store.Account{ProviderProfile: "microsoft"}, other) != other {
		t.Error("an unrelated error was rewritten")
	}
}
