package moverequest

import "testing"

func ptrStr(s string) *string { return &s }

func TestPlanAssignment(t *testing.T) {
	cases := []struct {
		name                       string
		curShift, curUser          *string
		reqShift, reqUser, reqType *string
		wantKind                   AssignmentKind
		wantShift, wantUser        string
	}{
		{"fields-only (nothing provided)", nil, nil, nil, nil, nil, AssignNoChange, "", ""},
		{"C: pending → user (manual)", nil, nil, nil, ptrStr("u1"), ptrStr("manual"), AssignToDriverKind, "", "u1"},
		{"D: user → shift (must null user)", nil, ptrStr("u1"), ptrStr("s1"), nil, ptrStr("shift"), AssignToShiftKind, "s1", ""},
		{"E: assigned → unassign via type=''", ptrStr("s1"), ptrStr("u1"), nil, nil, ptrStr(""), AssignUnassignKind, "", ""},
		{"no-change: re-send same shift", ptrStr("s1"), nil, ptrStr("s1"), nil, ptrStr("shift"), AssignNoChange, "", ""},
		{"no-change: re-send same user, no shift", nil, ptrStr("u1"), nil, ptrStr("u1"), ptrStr("manual"), AssignNoChange, "", ""},
		{"precedence: shift beats user", nil, nil, ptrStr("s1"), ptrStr("u1"), nil, AssignToShiftKind, "s1", ""},
		{"unassign no-op when already unassigned", nil, nil, nil, nil, ptrStr(""), AssignNoChange, "", ""},
		{"reassign to a different shift", ptrStr("s1"), nil, ptrStr("s2"), nil, ptrStr("shift"), AssignToShiftKind, "s2", ""},
	}
	for _, c := range cases {
		got := PlanAssignment(c.curShift, c.curUser, c.reqShift, c.reqUser, c.reqType)
		if got.Kind != c.wantKind || got.ShiftID != c.wantShift || got.UserID != c.wantUser {
			t.Errorf("%s: PlanAssignment = {kind=%v shift=%q user=%q}, want {kind=%v shift=%q user=%q}",
				c.name, got.Kind, got.ShiftID, got.UserID, c.wantKind, c.wantShift, c.wantUser)
		}
	}
}
