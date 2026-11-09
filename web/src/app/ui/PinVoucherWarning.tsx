// SPDX-License-Identifier: AGPL-3.0-only

// ADR-054 rule 8: the user trusted a key because the key they had vouched for it,
// and that earlier key has since been revoked (perhaps as compromised). One
// sentence, said where a key-changed prompt would be: the reader's signature line
// and the compose encrypt switch. It only tells; nothing here moves the pin.

import type { JSX } from "react";

import { t } from "../i18n";

export function PinVoucherWarning({ name, compromised }: { name: string; compromised: boolean }): JSX.Element {
  return (
    <p className="hint pin-voucher-warning" role="status">
      {t(compromised ? "keys.pinVouchedByCompromised" : "keys.pinVouchedByRevoked", { name })}
    </p>
  );
}
