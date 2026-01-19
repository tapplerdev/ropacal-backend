# Bin Priority & Retirement API - Complete ✅

## Overview
This document describes the bin priority scoring and retirement system that has been fully implemented in the Ropacal backend.

---

## 🎯 Priority Scoring System

### Endpoint: `GET /api/bins/priority`

Returns bins with dynamically calculated priority scores and supports filtering/sorting.

### Priority Calculation Algorithm

The priority score is calculated using multiple weighted factors:

```
Priority Score =
  Move Request Priority (0-1000) +
  Fill Percentage Priority (0-300) +
  Days Since Check Priority (0-1000) +
  Check Recommendation Flag (+100)
```

#### Scoring Breakdown:

**1. Move Requests (Highest Priority)**
- Urgent move request: +1000
- Scheduled move (tomorrow): +800
- Scheduled move (within 3 days): +600
- Scheduled move (within 7 days): +400
- Scheduled move (future): +100

**2. Fill Percentage**
- ≥80% full: +300
- ≥60% full: +150
- ≥40% full: +50

**3. Days Since Last Check**
- Never checked: +1000
- ≥30 days: +800
- ≥14 days: +400
- ≥7 days: +200

**4. Check Recommendations**
- Has pending check recommendation: +100

### Query Parameters

| Parameter | Values | Description |
|-----------|--------|-------------|
| `sort` | `priority` (default), `bin_number`, `fill_percentage`, `days_since_check` | Sort order |
| `filter` | `all` (default), `next_move_request`, `longest_unchecked`, `high_fill`, `has_check_recommendation` | Filter criteria |
| `status` | `active` (default), `all`, `retired`, `pending_move`, `in_storage` | Bin status filter |
| `limit` | number (default: 100) | Maximum results to return |

### Response Format

```json
[
  {
    "id": "uuid",
    "bin_number": 127,
    "current_street": "123 Main St",
    "city": "San Jose",
    "zip": "95110",
    "status": "active",
    "fill_percentage": 80,
    "latitude": 37.3382,
    "longitude": -121.8863,
    "priority_score": 850.0,
    "days_since_check": 14,
    "has_pending_move": false,
    "has_check_recommendation": true,
    "next_move_request_date": null,
    "move_request_urgency": null,
    "created_at": 1234567890,
    "updated_at": 1234567890
  }
]
```

### Example Requests

**1. Get top priority bins (default)**
```bash
GET /api/bins/priority
```

**2. Get all bins with pending move requests, sorted by priority**
```bash
GET /api/bins/priority?filter=next_move_request&sort=priority
```

**3. Get bins that haven't been checked in 7+ days**
```bash
GET /api/bins/priority?filter=longest_unchecked&limit=50
```

**4. Get high-fill bins sorted by fill percentage**
```bash
GET /api/bins/priority?filter=high_fill&sort=fill_percentage
```

**5. Get all active bins sorted by bin number**
```bash
GET /api/bins/priority?status=active&sort=bin_number&limit=200
```

---

## 🗑️ Bin Retirement System

### Endpoint: `POST /api/manager/bins/{id}/retire`

Marks a bin as retired or in storage.

### Request Body

```json
{
  "disposal_action": "retire",  // "retire" or "store"
  "reason": "Bin damaged beyond repair"  // Optional
}
```

### Disposal Actions

| Action | Status | Description |
|--------|--------|-------------|
| `retire` | `retired` | Bin is permanently retired from service |
| `store` | `in_storage` | Bin is temporarily stored, can be reactivated |

### Response Format

**Success (200 OK)**
```json
{
  "message": "Bin retired successfully",
  "status": "retired"
}
```

**Errors**
- `400 Bad Request` - Invalid disposal_action or missing bin ID
- `404 Not Found` - Bin not found or already retired
- `500 Internal Server Error` - Database error

### Database Schema

The following fields are automatically set when a bin is retired:

```sql
retired_at          BIGINT      -- Unix timestamp when retired
retired_by_user_id  TEXT        -- User ID who retired the bin (FK to users.id)
status              TEXT        -- 'retired' or 'in_storage'
updated_at          BIGINT      -- Updated timestamp
```

### Business Logic

1. Only `active` bins can be retired
2. Retirement is tracked with timestamp and user ID
3. Retired bins are excluded from route planning
4. Stored bins can potentially be reactivated (future feature)
5. Retirement reason is optional but recommended for record-keeping

### Example Requests

**1. Retire a damaged bin**
```bash
POST /api/manager/bins/abc123/retire
Content-Type: application/json
Authorization: Bearer <admin_token>

{
  "disposal_action": "retire",
  "reason": "Bin severely damaged, beyond repair"
}
```

**2. Store a bin temporarily**
```bash
POST /api/manager/bins/def456/retire
Content-Type: application/json
Authorization: Bearer <admin_token>

{
  "disposal_action": "store",
  "reason": "Seasonal storage - low demand area"
}
```

---

## 🔐 Authentication

Both endpoints require admin authentication:
- Priority endpoint: No auth required (read-only)
- Retirement endpoint: Requires `Authorization: Bearer <token>` header with admin role

---

## 🗄️ Database Migrations

### Migration: `add_bin_retirement_fields.sql`

```sql
-- Add retirement tracking fields
ALTER TABLE bins ADD COLUMN IF NOT EXISTS retired_at BIGINT;
ALTER TABLE bins ADD COLUMN IF NOT EXISTS retired_by_user_id TEXT;

-- Add foreign key constraint
ALTER TABLE bins ADD CONSTRAINT fk_bins_retired_by
    FOREIGN KEY (retired_by_user_id) REFERENCES users(id) ON DELETE SET NULL;

-- Add indexes for performance
CREATE INDEX IF NOT EXISTS idx_bins_retired_at ON bins(retired_at);
CREATE INDEX IF NOT EXISTS idx_bins_status ON bins(status);
```

**Status:** ✅ Complete - Ready to run

---

## 📊 Integration with Existing Systems

### Check Recommendations System
- Bins with pending check recommendations get +100 priority boost
- Integration via `bin_check_recommendations` table

### Move Request System
- Urgent moves get highest priority (+1000)
- Scheduled moves prioritized based on proximity to scheduled date
- Integration via `bin_move_requests` table

### Route Creation
- Priority scores can be used to intelligently select bins for routes
- High-priority bins should be included in next available route

---

## 🎯 Use Cases

### Dashboard Admin Panel
1. **Priority View:** Show bins sorted by priority score
2. **Filter Critical Bins:** Quickly identify urgent actions needed
3. **Retirement Management:** Easy interface to retire/store bins
4. **Route Planning:** Use priority data to optimize route creation

### Mobile App (Driver)
1. View assigned route bins sorted by priority
2. See why each bin is prioritized (move request, high fill, etc.)
3. Report bins that need retirement

---

## ✅ Implementation Status

| Feature | Status | Notes |
|---------|--------|-------|
| Priority calculation logic | ✅ Complete | Weighted scoring system |
| Priority API endpoint | ✅ Complete | GET /api/bins/priority |
| Filtering & sorting | ✅ Complete | Multiple filter/sort options |
| Retirement API endpoint | ✅ Complete | POST /api/manager/bins/{id}/retire |
| Database schema | ✅ Complete | Migration ready |
| Authentication | ✅ Complete | Admin role required for retirement |
| Compilation | ✅ Complete | No errors |

---

## 🚀 Next Steps

1. **Run Migration**
   ```bash
   # Migrations run automatically on server start
   # Or manually: psql $DATABASE_URL < migrations/add_bin_retirement_fields.sql
   ```

2. **Test Endpoints**
   ```bash
   # Test priority endpoint
   curl http://localhost:8080/api/bins/priority

   # Test retirement endpoint
   curl -X POST http://localhost:8080/api/manager/bins/{id}/retire \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer <token>" \
     -d '{"disposal_action":"retire","reason":"Test"}'
   ```

3. **Dashboard Integration**
   - Update bins table to use `/api/bins/priority` endpoint
   - Add priority score column (optional)
   - Connect "Retire Bin" modal to retirement endpoint
   - Add filters for high-priority bins

4. **Future Enhancements**
   - Un-retire bins workflow
   - Retirement history view
   - Bulk retirement operations
   - Priority score trending over time

---

## 📝 Notes

- Priority scores are calculated on-the-fly (not stored in database)
- This ensures scores are always up-to-date
- Performance is optimized with single query + in-memory calculation
- Retirement is permanent (requires manual database edit to undo)
- Consider implementing un-retire endpoint for reversibility
