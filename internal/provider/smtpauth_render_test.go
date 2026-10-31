// SPDX-License-Identifier: AGPL-3.0-only

package provider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
	"testing"

	"mailroom/internal/xoauth2"
)

// MEDIUM-1 (security review of #894): the XOAUTH2 mechanism and its net/smtp
// client keep the token in unexported fields, which fmt and slog render by
// reflection without calling methods. Every rendering of a SendRequest, of the
// mechanism, of the client, and of structs nesting them in unexported fields
// must show no token.
func TestXOAuth2SMTPNeverRendersTheToken(t *testing.T) {
	auth := XOAuth2Auth("you@example.com", xoauth2.StaticToken(smtpTestToken))
	cl, err := auth.client(context.Background(), "smtp.example.com")
	if err != nil {
		t.Fatal(err)
	}
	req := SendRequest{Host: "smtp.example.com", Auth: auth}
	type hidden struct {
		a SMTPAuth
		c smtp.Auth
		r SendRequest
	}
	if xc, ok := cl.(*xoauth2Client); !ok || xc.token.Get() != smtpTestToken {
		t.Fatal("control: the client does not hold the token, so this test would see nothing")
	}
	var outs []string
	for _, v := range []any{req, &req, auth, cl, hidden{auth, cl, req}, &hidden{auth, cl, req}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			outs = append(outs, fmt.Sprintf(verb, v))
		}
	}
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewTextHandler(&buf, nil), slog.NewJSONHandler(&buf, nil)} {
		slog.New(h).Info("send", "req", req, "auth", auth, "client", cl, "hidden", hidden{auth, cl, req})
	}
	outs = append(outs, buf.String())
	for _, o := range outs {
		if strings.Contains(o, smtpTestToken.Reveal()) {
			t.Errorf("the access token rendered: %s", o)
		}
	}
}
