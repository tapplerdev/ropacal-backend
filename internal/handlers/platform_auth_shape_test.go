package handlers

import (
	"encoding/json"
	"testing"
)

// The login response is a CONTRACT with three independent client checks
// (lib/auth/queries.ts, login-form.tsx, the auth store). All three branch on
// `platform` to skip the tenant shape, because an operator has no user and no
// organization. When the field went missing, every one of them correctly fell
// through and reported a fully successful login as malformed.
//
// Assert the wire bytes, not the struct fields — the failure was a JSON tag
// (`omitempty` swallowing a false bool), which a field-level check would miss.
func TestPlatformLoginSuccessShape(t *testing.T) {
	raw, err := json.Marshal(newPlatformLoginSuccess("tok", "op@binly.com", "Op", 123))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got["platform"] != true {
		t.Errorf("platform must be true on the wire, got %#v — clients treat its absence as a tenant login and reject the response", got["platform"])
	}
	if got["ok"] != true {
		t.Errorf("ok must be true, got %#v", got["ok"])
	}
	if got["token"] != "tok" {
		t.Errorf("token missing from response: %#v", got["token"])
	}

	// An operator is not a tenant user. If these ever appear, a client could
	// take the tenant branch and bind the session to an organization.
	for _, forbidden := range []string{"user", "organization"} {
		if _, present := got[forbidden]; present {
			t.Errorf("platform response must not carry %q", forbidden)
		}
	}
}

// The false case must still be visible, or a future omission silently vanishes
// again rather than showing up as platform:false.
func TestPlatformFlagSurvivesFalse(t *testing.T) {
	raw, _ := json.Marshal(PlatformLoginResponse{OK: true, Token: "t"})
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if _, present := got["platform"]; !present {
		t.Error("platform key disappeared when false — omitempty is back; that is the exact bug this guards")
	}
}
