package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPasswordResetRejectsInvalidInput(t *testing.T) {
	h := AuthHandler{}
	cases := []struct {
		name, body string
		forgot     bool
	}{
		{"malformed request", `{`, true},
		{"missing email", `{}`, true},
		{"invalid email", `{"email":"invalid"}`, true},
		{"display name", `{"email":"Name <name@example.com>"}`, true},
		{"oversized request", `{"email":"` + strings.Repeat("a", 5000) + `"}`, true},
		{"missing token", `{"password":"newpassword"}`, false},
		{"invalid token", `{"token":"not-hex","password":"newpassword"}`, false},
		{"short password", `{"token":"` + strings.Repeat("ab", 32) + `","password":"short"}`, false},
		{"long password", `{"token":"` + strings.Repeat("ab", 32) + `","password":"` + strings.Repeat("x", 73) + `"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/", strings.NewReader(tc.body))
			if tc.forgot {
				h.ForgotPassword(w, r)
			} else {
				h.ResetPassword(w, r)
			}
			if w.Code != 400 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestResetDigest(t *testing.T) {
	const expected = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := resetDigest("abc"); got != expected {
		t.Fatalf("unexpected SHA-256: %s", got)
	}
}
