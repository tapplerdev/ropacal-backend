# Move Request History Implementation Plan

## Overview
This document outlines all the handlers and locations that need to be updated to log move request history events.

---

## ✅ Completed
1. **Database Migration** - `move_request_history` table created
2. **Go Models** - `MoveRequestHistory` and `MoveRequestHistoryResponse` structs
3. **Helper Functions** - `internal/helpers/move_request_history.go` with logging functions

---

## 🔄 Handlers Requiring Updates

### 1. **Create Move Request** ✏️ PRIORITY: HIGH
**File**: `internal/handlers/bin_move_requests.go`
**Function**: `ScheduleBinMove` (line ~46)
**Action**: `created`
**When**: After successful INSERT into `bin_move_requests`
**Log Call**:
```go
helpers.LogMoveRequestCreated(db, moveRequestID, userID, userName)
```

---

### 2. **Assign to Shift** ✏️ PRIORITY: HIGH
**File**: `internal/handlers/bin_move_requests.go`
**Function**: `AssignMoveToShift` (line ~253) - Handler
**Function**: `assignMoveToShift` (line ~327) - Helper function
**Action**: `assigned` or `reassigned`
**When**: After UPDATE to `bin_move_requests` with `assigned_shift_id`
**Logic**:
- If `old assigned_shift_id == NULL` → Log `assigned`
- If `old assigned_shift_id != NULL` → Log `reassigned`

**Log Call**:
```go
// Check if reassignment or new assignment
if previousAssignedShiftID == nil {
    // New assignment
    helpers.LogMoveRequestAssigned(db, moveRequestID, actorID, actorName,
        "shift", &driverUserID, &driverName, &shiftID)
} else {
    // Reassignment
    helpers.LogMoveRequestReassigned(db, moveRequestID, actorID, actorName,
        previousAssignmentType, newAssignmentType,
        previousUserID, newUserID,
        previousUserName, newUserName,
        previousShiftID, newShiftID)
}
```

---

### 3. **Assign to User (Manual Assignment)** ✏️ PRIORITY: HIGH
**File**: `internal/handlers/bin_move_requests.go`
**Function**: `AssignMoveToUser` (line ~1834)
**Action**: `assigned` (manual assignment)
**When**: After UPDATE to `bin_move_requests` with `assigned_user_id`
**Log Call**:
```go
helpers.LogMoveRequestAssigned(db, moveRequestID, actorID, actorName,
    "manual", &userID, &userName, nil)
```

---

### 4. **Clear Assignment (Unassign)** ✏️ PRIORITY: MEDIUM
**File**: `internal/handlers/bin_move_requests.go`
**Function**: `ClearMoveAssignment` (line ~2152)
**Action**: `unassigned`
**When**: After UPDATE that sets `assigned_shift_id` and `assigned_user_id` to NULL
**Log Call**:
```go
helpers.LogMoveRequestUnassigned(db, moveRequestID, actorID, actorName,
    previousAssignmentType, previousUserID, previousUserName, previousShiftID)
```

---

### 5. **Update Move Request Details** ✏️ PRIORITY: MEDIUM
**File**: `internal/handlers/bin_move_requests.go`
**Function**: `UpdateBinMoveRequest` (line ~1096)
**Action**: `updated` or `reassigned` (if assignment changed)
**When**: After UPDATE to `bin_move_requests`
**Logic**: Check what changed:
- If assignment changed → Log `reassigned`
- Otherwise → Log `updated`

**Log Call**:
```go
// Check if assignment changed
if assignmentChanged {
    helpers.LogMoveRequestReassigned(db, moveRequestID, actorID, actorName, ...)
} else {
    notes := "Updated move details (location/date/notes)"
    helpers.LogMoveRequestUpdated(db, moveRequestID, actorID, actorName, &notes)
}
```

---

### 6. **Complete Move Request (Driver)** ✏️ PRIORITY: HIGH
**File**: `internal/handlers/shifts.go`
**Function**: `handleMoveRequestCompletion` (line ~2352)
**Action**: `completed`
**When**: After UPDATE that sets `status = 'completed'` and `completed_at`
**Log Call**:
```go
helpers.LogMoveRequestCompleted(db, moveRequestID, driverUserID, driverName)
```

---

### 7. **Manually Complete Move Request (Manager)** ✏️ PRIORITY: HIGH
**File**: `internal/handlers/bin_move_requests.go`
**Function**: `ManuallyCompleteMoveRequest` (line ~1971)
**Action**: `completed`
**When**: After UPDATE that sets `status = 'completed'`
**Log Call**:
```go
helpers.LogMoveRequestCompleted(db, moveRequestID, managerUserID, managerName)
```

---

### 8. **Cancel Move Request** ✏️ PRIORITY: HIGH
**File**: `internal/handlers/bin_move_requests.go`
**Function**: `CancelBinMoveRequest` (line ~1748)
**Action**: `cancelled`
**When**: After UPDATE that sets `status = 'cancelled'`
**Log Call**:
```go
reason := "Cancelled by manager"
helpers.LogMoveRequestCancelled(db, moveRequestID, managerUserID, managerName, &reason)
```

---

## 🆕 New API Endpoint

### 9. **Get Move Request History** ✏️ PRIORITY: HIGH
**File**: `internal/handlers/bin_move_requests.go`
**Endpoint**: `GET /api/move-requests/:id/history` or `GET /api/manager/bins/move-requests/:id/history`
**Purpose**: Retrieve full audit trail for a move request
**Function Name**: `GetMoveRequestHistory`

**Implementation**:
```go
func GetMoveRequestHistory(db *sqlx.DB) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Extract move request ID from URL
        id := chi.URLParam(r, "id")

        // Get history using helper
        history, err := helpers.GetMoveRequestHistory(db, id)
        if err != nil {
            http.Error(w, "Failed to fetch history", http.StatusInternalServerError)
            return
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(history)
    }
}
```

**Register Route** in `cmd/server/main.go`:
```go
r.Get("/api/manager/bins/move-requests/{id}/history", handlers.GetMoveRequestHistory(db))
```

---

## 📝 Implementation Order

### Phase 1: Critical Actions (Do First)
1. ✅ Create Move Request - `ScheduleBinMove`
2. ✅ Complete Move Request (Driver) - `handleMoveRequestCompletion`
3. ✅ Complete Move Request (Manager) - `ManuallyCompleteMoveRequest`
4. ✅ Cancel Move Request - `CancelBinMoveRequest`
5. ✅ Create API Endpoint - `GetMoveRequestHistory`

### Phase 2: Assignment Actions
6. ✅ Assign to Shift - `AssignMoveToShift`
7. ✅ Assign to User - `AssignMoveToUser`
8. ✅ Clear Assignment - `ClearMoveAssignment`

### Phase 3: Update Actions
9. ✅ Update Move Request - `UpdateBinMoveRequest` (track reassignments here too)

---

## 🧪 Testing Checklist

After implementation, test the following scenarios:

- [ ] Create a move request → Check history shows "created"
- [ ] Assign to shift → Check history shows "assigned to [driver]"
- [ ] Reassign to different shift → Check history shows "reassigned from [driver1] to [driver2]"
- [ ] Assign manually to user → Check history shows "manually assigned to [user]"
- [ ] Unassign → Check history shows "unassigned from [driver/user]"
- [ ] Complete move (driver) → Check history shows "completed by [driver]"
- [ ] Complete move (manager) → Check history shows "completed by [manager]"
- [ ] Cancel move → Check history shows "cancelled by [manager]"
- [ ] Update move details → Check history shows "updated"
- [ ] Call GET /api/move-requests/:id/history → Verify timeline returned

---

## 📊 Expected History Timeline Example

```json
[
  {
    "id": "uuid1",
    "action_type": "created",
    "action_type_label": "Created",
    "actor_name": "Manager Nate",
    "description": "Created move request",
    "created_at_iso": "2026-01-27T12:25:00Z"
  },
  {
    "id": "uuid2",
    "action_type": "assigned",
    "action_type_label": "Assigned",
    "actor_name": "Manager Nate",
    "description": "Assigned to John Driver",
    "new_assigned_user_name": "John Driver",
    "created_at_iso": "2026-01-27T12:30:00Z"
  },
  {
    "id": "uuid3",
    "action_type": "reassigned",
    "action_type_label": "Reassigned",
    "actor_name": "Manager Sarah",
    "description": "Reassigned from John Driver to Mike Smith",
    "previous_assigned_user_name": "John Driver",
    "new_assigned_user_name": "Mike Smith",
    "created_at_iso": "2026-01-27T13:15:00Z"
  },
  {
    "id": "uuid4",
    "action_type": "completed",
    "action_type_label": "Completed",
    "actor_name": "Mike Smith",
    "description": "Completed",
    "created_at_iso": "2026-01-27T14:30:00Z"
  }
]
```

---

## 🚀 Ready to Implement

All helper functions are ready in `internal/helpers/move_request_history.go`.

Next step: Update handlers one by one according to the priority order above.
