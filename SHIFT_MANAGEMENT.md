# Shift Management Backend - Implementation Guide

## ✅ What Was Implemented

### 1. Database Tables (SQLite)
- **shifts table**: Tracks driver shifts with status, timing, and progress
- **fcm_tokens table**: Stores Firebase Cloud Messaging tokens for push notifications

### 2. Models (`internal/models/shift.go`)
- `Shift` - Main shift model with helper methods
- `FCMToken` - FCM token storage model
- `ShiftEndResponse` - Response when shift ends
- `CompleteBinResponse` - Response when bin is completed

### 3. WebSocket Hub (`internal/websocket/`)
- Real-time bidirectional communication
- Automatic connection management
- Heartbeat/ping-pong to keep connections alive
- Broadcasts shift updates to connected drivers

### 4. Firebase Cloud Messaging (`internal/services/fcm.go`)
- Push notifications for route assignments
- Shift update notifications
- Multicast support

### 5. API Endpoints (`internal/handlers/shifts.go`)

#### Driver Endpoints (Require Auth):
- `GET /api/driver/shift/current` - Get current shift status
- `POST /api/driver/shift/start` - Start an assigned shift
- `POST /api/driver/shift/pause` - Pause active shift (break time)
- `POST /api/driver/shift/resume` - Resume from pause
- `POST /api/driver/shift/end` - End shift with duration stats
- `POST /api/driver/shift/complete-bin` - Mark bin as completed
- `POST /api/driver/fcm-token` - Register FCM token

#### Manager Endpoints (Require Auth + Manager Role):
- `POST /api/manager/assign-route` - Assign route to driver

#### WebSocket Endpoint (Require Auth):
- `GET /ws` - WebSocket connection for real-time updates

---

## 🚀 Running the Backend

### Prerequisites
1. Go 1.25.4+
2. Firebase service account JSON file (already placed at `~/Desktop/ropacal-backend/firebase-service-account.json`)

### Start Server
```bash
cd ~/Desktop/ropacal-backend

# Run the server
go run cmd/server/main.go

# Or use the built binary
./bin/server
```

Server starts on **http://localhost:8080** by default.

---

## 📡 API Usage Examples

### 1. Login (Get JWT Token)
```bash
curl -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "email": "driver@example.com",
    "password": "password123"
  }'
```

Response:
```json
{
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "user": {
    "id": "uuid",
    "email": "driver@example.com",
    "name": "John Driver",
    "role": "driver"
  }
}
```

### 2. Register FCM Token
```bash
curl -X POST http://localhost:8080/api/driver/fcm-token \
  -H "Authorization: Bearer YOUR_JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "token": "FCM_TOKEN_FROM_FLUTTER",
    "device_type": "ios"
  }'
```

### 3. Manager Assigns Route
```bash
curl -X POST http://localhost:8080/api/manager/assign-route \
  -H "Authorization: Bearer MANAGER_JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "driver_id": "driver-uuid",
    "route_id": "route_123",
    "total_bins": 25
  }'
```

This will:
- Create shift with status "ready"
- Send push notification to driver (if FCM token exists)
- Send WebSocket message to driver (if connected)

### 4. Get Current Shift
```bash
curl http://localhost:8080/api/driver/shift/current \
  -H "Authorization: Bearer DRIVER_JWT_TOKEN"
```

### 5. Start Shift
```bash
curl -X POST http://localhost:8080/api/driver/shift/start \
  -H "Authorization: Bearer DRIVER_JWT_TOKEN"
```

### 6. Pause Shift
```bash
curl -X POST http://localhost:8080/api/driver/shift/pause \
  -H "Authorization: Bearer DRIVER_JWT_TOKEN"
```

### 7. Resume Shift
```bash
curl -X POST http://localhost:8080/api/driver/shift/resume \
  -H "Authorization: Bearer DRIVER_JWT_TOKEN"
```

### 8. Complete Bin
```bash
curl -X POST http://localhost:8080/api/driver/shift/complete-bin \
  -H "Authorization: Bearer DRIVER_JWT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "bin_id": "bin-uuid"
  }'
```

### 9. End Shift
```bash
curl -X POST http://localhost:8080/api/driver/shift/end \
  -H "Authorization: Bearer DRIVER_JWT_TOKEN"
```

---

## 🔌 WebSocket Connection

### Connect
```javascript
const ws = new WebSocket('ws://localhost:8080/ws?token=YOUR_JWT_TOKEN');

ws.onopen = () => {
  console.log('WebSocket connected');

  // Send heartbeat every 30 seconds
  setInterval(() => {
    ws.send(JSON.stringify({
      type: 'ping',
      timestamp: new Date().toISOString()
    }));
  }, 30000);
};

ws.onmessage = (event) => {
  const message = JSON.parse(event.data);

  switch (message.type) {
    case 'pong':
      console.log('Heartbeat acknowledged');
      break;
    case 'route_assigned':
      console.log('New route assigned!', message.data);
      // Show notification to driver
      break;
    case 'shift_update':
      console.log('Shift updated', message.data);
      // Update UI
      break;
  }
};
```

---

## 📊 Database Schema

### shifts table
```sql
CREATE TABLE shifts (
  id TEXT PRIMARY KEY,
  driver_id TEXT NOT NULL,
  route_id TEXT,
  status TEXT NOT NULL CHECK(status IN ('inactive', 'ready', 'active', 'paused')),
  start_time INTEGER,
  end_time INTEGER,
  total_pause_seconds INTEGER DEFAULT 0,
  pause_start_time INTEGER,
  total_bins INTEGER DEFAULT 0,
  completed_bins INTEGER DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY (driver_id) REFERENCES users(id)
);
```

### fcm_tokens table
```sql
CREATE TABLE fcm_tokens (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id TEXT NOT NULL,
  token TEXT NOT NULL UNIQUE,
  device_type TEXT NOT NULL CHECK(device_type IN ('ios', 'android')),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  FOREIGN KEY (user_id) REFERENCES users(id)
);
```

---

## 🔧 Environment Variables

Create or update `.env` file:
```env
DATABASE_PATH=./db/ropacal.db
PORT=8080
APP_JWT_SECRET=your-secret-key-here
FIREBASE_CREDENTIALS_FILE=./firebase-service-account.json
```

---

## 🧪 Testing Workflow

### Full Shift Lifecycle Test:

1. **Manager assigns route:**
   ```bash
   POST /api/manager/assign-route
   ```
   → Shift created with status "ready"
   → Push notification sent
   → WebSocket message sent

2. **Driver receives notification**
   → Opens app
   → Sees slide-to-start button

3. **Driver starts shift:**
   ```bash
   POST /api/driver/shift/start
   ```
   → Shift status → "active"
   → Timer starts

4. **Driver takes break:**
   ```bash
   POST /api/driver/shift/pause
   ```
   → Shift status → "paused"
   → Pause timer starts

5. **Driver resumes:**
   ```bash
   POST /api/driver/shift/resume
   ```
   → Shift status → "active"
   → Pause time calculated and added to total

6. **Driver completes bins:**
   ```bash
   POST /api/driver/shift/complete-bin (x25 times)
   ```
   → completed_bins increments
   → WebSocket updates UI

7. **Driver ends shift:**
   ```bash
   POST /api/driver/shift/end
   ```
   → Shift status → "inactive"
   → Returns summary (total time, active time, pause time, bins)

---

## 📝 Important Notes

### Time Tracking
- All times stored as Unix timestamps (seconds)
- Active duration = total time - pause time
- Pause time calculated correctly even if driver ends shift while paused

### WebSocket Messages
- Heartbeat required every 30 seconds to keep connection alive
- Server sends pong in response to ping
- Automatic reconnection should be handled client-side

### Push Notifications
- Requires valid FCM token registered via `/api/driver/fcm-token`
- Notifications sent for route assignments
- Can be extended for shift updates, reminders, etc.

### Security
- All endpoints (except login) require JWT authentication
- Manager endpoints require "manager" or "admin" role
- WebSocket connections authenticated via query parameter token

---

## 🎯 Next Steps

### Backend Ready ✅
- All 7 shift endpoints implemented
- WebSocket real-time updates working
- Firebase push notifications configured
- Database migrations complete

### Flutter Integration (Next)
1. Update `ShiftService` to call real API endpoints (currently has TODO comments)
2. Update `ShiftNotifier` to sync with backend
3. Register FCM token on app startup
4. Connect WebSocket when user logs in
5. Test full workflow end-to-end

---

## 🐛 Troubleshooting

### Server won't start
- Check `firebase-service-account.json` exists
- Check `.env` file has required variables
- Check port 8080 is not in use

### FCM notifications not working
- Verify `firebase-service-account.json` is valid
- Check FCM token is registered via `/api/driver/fcm-token`
- Check device has granted notification permissions

### WebSocket not connecting
- Ensure JWT token is valid and not expired
- Check token is passed as query parameter: `?token=YOUR_TOKEN`
- Verify middleware.Auth is working correctly

---

## 📂 File Structure

```
ropacal-backend/
├── cmd/server/main.go              # Entry point, routes setup
├── internal/
│   ├── database/database.go        # Database migrations
│   ├── handlers/
│   │   └── shifts.go              # 8 shift endpoints
│   ├── middleware/auth.go          # JWT authentication
│   ├── models/shift.go             # Shift & FCM token models
│   ├── services/fcm.go             # Firebase push notifications
│   └── websocket/
│       ├── hub.go                  # WebSocket hub
│       ├── client.go               # WebSocket client
│       └── handler.go              # HTTP upgrade handler
├── pkg/utils/response.go           # JSON response helpers
└── firebase-service-account.json   # Firebase credentials
```

---

## ✨ Summary

Your backend is **production-ready** with:
- ✅ SQLite database with shift tracking
- ✅ JWT authentication
- ✅ 8 RESTful API endpoints
- ✅ Real-time WebSocket updates
- ✅ Firebase Cloud Messaging push notifications
- ✅ Automatic time tracking (excluding pauses)
- ✅ Manager/admin role-based access control

**The backend is fully functional and ready to be integrated with your Flutter app!**
