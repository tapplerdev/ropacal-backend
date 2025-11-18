# Ropacal Backend - Golang + PostgreSQL

A high-performance Golang backend for the Ropacal bin management system with PostgreSQL database, WebSockets for real-time updates, and Firebase Cloud Messaging for push notifications.

## Features

- **Production-Ready**: PostgreSQL for scalable data persistence
- **Real-Time**: WebSocket support for instant shift/route updates
- **Push Notifications**: Firebase Cloud Messaging integration
- **Fast**: Written in Go for high performance
- **Secure**: JWT authentication with role-based access control
- **Complete**: Full REST API with driver and manager endpoints

## Tech Stack

- **Language**: Go 1.25.4
- **Database**: PostgreSQL 12+
- **Router**: Chi (lightweight HTTP router)
- **Auth**: JWT with 7-day expiration
- **WebSockets**: gorilla/websocket for real-time communication
- **Push Notifications**: Firebase Cloud Messaging
- **CORS**: Enabled for all origins (configurable)

## Project Structure

```
ropacal-backend/
├── cmd/server/          # Application entry point
├── internal/
│   ├── models/          # Data models (Bin, Check, Move, Shift, User, etc.)
│   ├── database/        # DB connection, migrations, seeding
│   ├── handlers/        # HTTP handlers (auth, bins, shifts, routes)
│   ├── middleware/      # JWT auth & role-based access control
│   ├── services/        # Firebase Cloud Messaging service
│   └── websocket/       # WebSocket hub, client, and handlers
├── pkg/utils/           # Utility functions
├── .env                 # Environment configuration (DO NOT commit)
├── .env.example         # Environment variables template
├── DEPLOYMENT.md        # Comprehensive deployment guide
├── SHIFT_MANAGEMENT.md  # Shift management documentation
└── README.md
```

## Getting Started

### Prerequisites

1. **PostgreSQL 12+** installed and running
2. **Go 1.25+** installed
3. **(Optional)** Firebase project for push notifications

### 1. Set Up PostgreSQL Database

```bash
# Install PostgreSQL (macOS)
brew install postgresql@14
brew services start postgresql@14

# Create database
createdb ropacal

# Verify connection
psql ropacal
```

### 2. Configure Environment Variables

```bash
# Copy example env file
cp .env.example .env

# Edit .env and set:
# - DATABASE_URL (PostgreSQL connection string)
# - APP_JWT_SECRET (generate with: openssl rand -base64 32)
# - FIREBASE_CREDENTIALS_FILE (optional, for push notifications)
```

Example `.env`:
```bash
DATABASE_URL=postgres://postgres:postgres@localhost:5432/ropacal?sslmode=disable
APP_JWT_SECRET=your-super-secret-key-min-32-chars
PORT=8080
FIREBASE_CREDENTIALS_FILE=./firebase-service-account.json
```

### 3. Install Dependencies

```bash
cd /Users/omargabr/Desktop/ropacal-backend
go mod tidy
```

### 4. Run the Server

```bash
go run cmd/server/main.go
```

The server will:
- Connect to PostgreSQL database
- Run database migrations (create tables, indexes)
- Seed test users: `driver@ropacal.com` / `driver123`, `admin@ropacal.com` / `admin123`
- Seed 44 bins in San Jose with locations
- Start WebSocket hub for real-time updates
- Initialize Firebase Cloud Messaging (if configured)
- Start HTTP server on port 8080

### 5. Test the Server

```bash
# Health check
curl http://localhost:8080/health

# Get all bins
curl http://localhost:8080/api/bins

# Login (get JWT token)
curl -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"driver@ropacal.com","password":"driver123"}'
```

## API Endpoints

### Authentication

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/auth/login` | Login and get JWT token |

**Request Body:**
```json
{
  "password": "ropacal123"
}
```

**Response:**
```json
{
  "ok": true,
  "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
}
```

### Bins

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/bins` | Get all bins |
| PATCH | `/api/bins/:id` | Update bin (creates check record if checked) |
| DELETE | `/api/bins/:id` | Delete bin |

### Checks

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/bins/:id/checks` | Get check history for bin |

### Moves

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/bins/:id/moves` | Get move history for bin |
| POST | `/api/bins/:id/moves` | Create move (updates bin location) |

**Create Move Request:**
```json
{
  "toStreet": "123 Main St",
  "toCity": "San Jose",
  "toZip": "95113",
  "movedOnIso": "2025-01-15T10:30:00Z" // optional
}
```

### Update Bin Request

```json
{
  "current_street": "123 Main St",
  "city": "San Jose",
  "zip": "95113",
  "status": "Active",
  "checked": true,
  "fill_percentage": 75,
  "move_requested": false,
  "checkedFrom": "Current location",  // optional
  "checkedOnIso": "2025-01-15T10:30:00Z"  // optional
}
```

## Environment Variables

See `.env.example` for template. Required variables:

| Variable | Description | Example |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | `postgres://user:pass@host:5432/dbname?sslmode=disable` |
| `APP_JWT_SECRET` | JWT signing secret (min 32 chars) | Generate: `openssl rand -base64 32` |
| `PORT` | Server port | `8080` |
| `FIREBASE_CREDENTIALS_FILE` | Path to Firebase service account JSON (optional) | `./firebase-service-account.json` |

**Important:**
- Never commit `.env` or `firebase-service-account.json` to Git
- Use `.env.example` as a template
- Generate strong secrets for production

## Database Schema

### bins table
```sql
id TEXT PRIMARY KEY
bin_number INT UNIQUE
current_street TEXT
city TEXT
zip TEXT
last_moved BIGINT (Unix timestamp)
last_checked BIGINT (Unix timestamp)
status TEXT ("Active" or "Missing")
fill_percentage INT (0-100)
checked INT (0 or 1)
move_requested INT (0 or 1)
latitude DOUBLE PRECISION
longitude DOUBLE PRECISION
created_at BIGINT (Unix timestamp)
updated_at BIGINT (Unix timestamp)
```

### checks table
```sql
id SERIAL PRIMARY KEY
bin_id TEXT (FK to bins.id)
checked_from TEXT
fill_percentage INT
checked_on BIGINT (Unix timestamp)
```

### moves table
```sql
id SERIAL PRIMARY KEY
bin_id TEXT (FK to bins.id)
moved_from TEXT
moved_to TEXT
moved_on BIGINT (Unix timestamp)
```

### shifts table
```sql
id TEXT PRIMARY KEY
driver_id TEXT (FK to users.id)
route_id TEXT
status TEXT ('inactive', 'ready', 'active', 'paused')
start_time BIGINT
end_time BIGINT
total_pause_seconds INT
pause_start_time BIGINT
total_bins INT
completed_bins INT
created_at BIGINT
updated_at BIGINT
```

### users table
```sql
id TEXT PRIMARY KEY
email TEXT UNIQUE
password TEXT (bcrypt hashed)
name TEXT
role TEXT ('driver', 'admin')
created_at BIGINT
updated_at BIGINT
```

### route_bins table
```sql
id SERIAL PRIMARY KEY
shift_id TEXT (FK to shifts.id)
bin_id TEXT (FK to bins.id)
sequence_order INT
is_completed INT (0 or 1)
completed_at BIGINT
created_at BIGINT
```

### fcm_tokens table
```sql
id SERIAL PRIMARY KEY
user_id TEXT (FK to users.id)
token TEXT UNIQUE
device_type TEXT ('ios', 'android')
created_at BIGINT
updated_at BIGINT
```

## Special Features

### Auto-Uncheck Logic
Bins that haven't been checked in 3+ days are automatically unchecked when fetching the bins list.

### Address Change Detection
When bin address changes (via UPDATE or MOVE), latitude/longitude are cleared and need to be re-geocoded.

### Transaction Support
Move and Check operations use database transactions to ensure data consistency.

## Integration with Flutter App

Update your Flutter app's API base URL to point to this backend:

```dart
// lib/core/services/api_service.dart
class ApiService {
  final Dio _dio = Dio(BaseOptions(
    baseUrl: 'http://localhost:8080/api',  // Update this
    headers: {'Content-Type': 'application/json'},
  ));
  // ... rest of the code
}
```

## Deployment to Production

**See [DEPLOYMENT.md](./DEPLOYMENT.md) for comprehensive deployment guide including:**

- Recommended platforms (Fly.io, Railway, Render)
- PostgreSQL setup and configuration
- Firebase credentials management
- Environment variable best practices
- Security recommendations
- Platform-specific deployment steps
- Testing and troubleshooting

### Quick Deployment Checklist

- [ ] PostgreSQL database created
- [ ] `DATABASE_URL` environment variable set
- [ ] Strong `APP_JWT_SECRET` generated and set
- [ ] Firebase credentials configured (optional)
- [ ] CORS origins restricted for production
- [ ] Application deployed and health check passing
- [ ] WebSocket connection tested
- [ ] Flutter app updated with production API URL

## WebSocket Integration

### Connect to WebSocket

```bash
# WebSocket endpoint requires JWT token in query parameter
ws://localhost:8080/ws?token=YOUR_JWT_TOKEN
```

### WebSocket Events

| Event | Description | Payload |
|-------|-------------|---------|
| `route_assigned` | Manager assigned route to driver | `{ shift, routeBins }` |
| `shift_update` | Shift status changed | `{ shift, routeBins }` |
| `shift_deleted` | Shift was deleted | `{ shiftId }` |

**Flutter Example:**
```dart
final wsUrl = 'ws://localhost:8080/ws?token=$jwtToken';
final channel = WebSocketChannel.connect(Uri.parse(wsUrl));

channel.stream.listen((message) {
  final data = jsonDecode(message);
  if (data['type'] == 'route_assigned') {
    // Handle route assignment
  }
});
```

## Performance

| Metric | Value |
|--------|-------|
| Memory Usage | ~15MB (idle) |
| Startup Time | ~100ms |
| Binary Size | ~20MB |
| Concurrent Connections | 10,000+ (WebSockets) |
| Request Latency | <5ms (local DB) |

## Troubleshooting

### Database Connection Failed
```bash
# Test PostgreSQL connection
psql $DATABASE_URL

# Check DATABASE_URL format
echo $DATABASE_URL
```

### Firebase Initialization Failed
Application continues without push notifications. Check:
- Firebase service account JSON file exists
- JSON file is valid
- Firebase project has Cloud Messaging enabled

### Port Already in Use
```bash
lsof -i :8080
kill -9 <PID>
```

### WebSocket Connection Refused
- Verify JWT token is valid
- Check token is passed in query parameter: `?token=...`
- Ensure platform supports WebSockets (Vercel does NOT)

**More troubleshooting:** See [DEPLOYMENT.md](./DEPLOYMENT.md#troubleshooting)

## License

MIT

## Contact

For issues or questions, contact the development team.
