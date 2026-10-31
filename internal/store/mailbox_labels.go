// SPDX-License-Identifier: AGPL-3.0-only

package store

import (
	"context"
	"fmt"

	"github.com/oklog/ulid/v2"
)

// MailboxLabel is what an operator report needs to name a mailbox: the account
// it belongs to, its JMAP role ("" when it has none) and its raw IMAP name.
type MailboxLabel struct {
	AccountID ulid.ULID
	Role      string
	IMAPName  string
}

// MailboxLabels reads every mailbox of the caller in one query, for the admin
// reports (`auth-report --authserv --no-ar`). The role comes from
// roleForMailbox, the function every other read path uses. READ-ONLY.
func (tx *UserTx) MailboxLabels(ctx context.Context) (map[ulid.ULID]MailboxLabel, error) {
	rows, err := tx.tx.Query(ctx, `
		SELECT id, account_id, imap_name, COALESCE(special_use, '')
		  FROM app.mailboxes
		 WHERE user_id = $1`, tx.userID.String())
	if err != nil {
		return nil, fmt.Errorf("store: mailbox labels: %w", err)
	}
	defer rows.Close()
	out := map[ulid.ULID]MailboxLabel{}
	for rows.Next() {
		var idStr, accStr, name, special string
		if err := rows.Scan(&idStr, &accStr, &name, &special); err != nil {
			return nil, fmt.Errorf("store: scan mailbox label: %w", err)
		}
		id, err := ulid.Parse(idStr)
		if err != nil {
			return nil, fmt.Errorf("store: mailbox label: id %q: %w", idStr, err)
		}
		acc, err := ulid.Parse(accStr)
		if err != nil {
			return nil, fmt.Errorf("store: mailbox label: account id %q: %w", accStr, err)
		}
		out[id] = MailboxLabel{AccountID: acc, Role: roleForMailbox(name, special), IMAPName: name}
	}
	return out, rows.Err()
}
