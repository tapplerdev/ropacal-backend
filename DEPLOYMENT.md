# Ropacal Backend - Deployment Guide

## Overview

This guide covers deploying the Ropacal backend to production environments. The backend is built with Go and uses PostgreSQL for data persistence, WebSockets for real-time communication, and Firebase Cloud Messaging for push notifications.

---

## Prerequisites

- PostgreSQL database (version 12 or higher)
- Firebase project with Cloud Messaging enabled
- Go 1.25+ (for local development)

---

## Environment Variables

### Required Environment Variables

| Variable | Description | Example |
|----------|-------------|---------|
| `DATABASE_URL` | PostgreSQL connection string | `postgres://user:pass@host:5432/dbname?sslmode=require` |
| `APP_JWT_SECRET` | Secret key for JWT token signing | `your-super-secret-key-min-32-chars` |
| `PORT` | Server port (defaults to 8080) | `8080` |

### Optional Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `FIREBASE_CREDENTIALS_FILE` | Path to Firebase service account JSON | `./firebase-service-account.json` |
| `APP_SHARED_PASSWORD` | Shared password for testing | `ropacal123` |

---

## Database Configuration

### PostgreSQL Connection String Format

```
postgres://username:password@hostname:port/database_name?sslmode=require
```

**Components:**
- `username`: Database user
- `password`: Database password
- `hostname`: Database host (e.g., `localhost`, `db.example.com`)
- `port`: Database port (default: `5432`)
- `database_name`: Name of your database
- `sslmode`: SSL mode (`disable`, `require`, `verify-ca`, `verify-full`)

**Examples:**

Local development:
```bash
DATABASE_URL=postgres://postgres:postgres@localhost:5432/ropacal?sslmode=disable
```

Production (Fly.io, Railway, Render):
```bash
DATABASE_URL=postgres://user:pass@mydb.internal:5432/ropacal?sslmode=require
```

### Database Initialization

The application automatically:
1. Connects to PostgreSQL on startup
2. Runs schema migrations (creates tables, indexes)
3. Seeds initial data (test users and 44 bins in San Jose)

**Default Test Users:**
- Driver: `driver@ropacal.com` / `driver123`
- Admin: `admin@ropacal.com` / `admin123`

---

## Firebase Cloud Messaging (Push Notifications)

### Setup Firebase Credentials

#### Option 1: Service Account File (Recommended for Testing)

1. Go to [Firebase Console](https://console.firebase.google.com/)
2. Select your project
3. Navigate to **Project Settings** → **Service Accounts**
4. Click **Generate New Private Key**
5. Download the JSON file

**For local development:**
- Save as `firebase-service-account.json` in project root
- Set environment variable:
  ```bash
  FIREBASE_CREDENTIALS_FILE=./firebase-service-account.json
  ```

**For production deployment:**
- **DO NOT** commit this file to Git
- Add to `.gitignore`
- Use platform-specific secrets management (see below)

#### Option 2: Environment Variable (Recommended for Production)

Most platforms support base64-encoded credentials:

```bash
# Encode your service account JSON
cat firebase-service-account.json | base64

# Set as environment variable
FIREBASE_CREDENTIALS_BASE64=<base64-encoded-json>
```

Then update `cmd/server/main.go` to decode:
```go
// Add this helper function
func loadFirebaseCredsFromEnv() ([]byte, error) {
    encoded := os.Getenv("FIREBASE_CREDENTIALS_BASE64")
    if encoded == "" {
        return nil, fmt.Errorf("FIREBASE_CREDENTIALS_BASE64 not set")
    }
    return base64.StdEncoding.DecodeString(encoded)
}
```

### Handling Missing Firebase Credentials

The application gracefully handles missing Firebase credentials:
- Push notifications will be disabled
- WebSocket updates will still work
- Logs warning: `⚠️ Failed to initialize FCM: ... (push notifications disabled)`

---

## Security Best Practices

### 1. JWT Secret

Generate a strong secret (minimum 32 characters):

```bash
# Generate random secret
openssl rand -base64 32
```

Set in environment:
```bash
APP_JWT_SECRET=<generated-secret>
```

### 2. Database Credentials

**Never** commit database credentials to Git:
- Use `.env` for local development
- Add `.env` to `.gitignore`
- Use platform environment variables for production

### 3. Firebase Service Account

**Critical:**
- Never commit `firebase-service-account.json` to Git
- Rotate keys if accidentally exposed
- Use platform secrets management (see deployment platforms below)

### 4. CORS Configuration

**Current setting (for testing):**
```go
AllowedOrigins: []string{"*"}
```

**Production recommendation:**
```go
AllowedOrigins: []string{
    "https://yourapp.com",
    "https://www.yourapp.com",
}
```

Update in `cmd/server/main.go:79-86`

---

## Deployment Platforms

### Option 1: Fly.io (Recommended)

**Why Fly.io:**
- Native WebSocket support
- Persistent volumes for PostgreSQL
- Global edge network
- Free tier includes 3 shared VMs + 3GB persistent storage

**Setup:**

1. Install Fly CLI:
   ```bash
   curl -L https://fly.io/install.sh | sh
   ```

2. Login and create app:
   ```bash
   fly auth login
   cd /Users/omargabr/Desktop/ropacal-backend
   fly launch
   ```

3. Create PostgreSQL database:
   ```bash
   fly postgres create
   fly postgres attach <postgres-app-name>
   ```

4. Set environment variables:
   ```bash
   fly secrets set APP_JWT_SECRET="your-secret-key"
   fly secrets set FIREBASE_CREDENTIALS_BASE64="<base64-encoded-json>"
   ```

5. Create `fly.toml`:
   ```toml
   app = "ropacal-backend"
   primary_region = "sjc"

   [build]
     builder = "paketobuildpacks/builder:base"

   [env]
     PORT = "8080"

   [[services]]
     internal_port = 8080
     protocol = "tcp"

     [[services.ports]]
       port = 80
       handlers = ["http"]

     [[services.ports]]
       port = 443
       handlers = ["http", "tls"]
   ```

6. Deploy:
   ```bash
   fly deploy
   ```

**Access your API:**
```
https://ropacal-backend.fly.dev
```

---

### Option 2: Railway

**Why Railway:**
- Very developer-friendly UI
- One-click PostgreSQL provisioning
- WebSocket support
- $5/month free credit

**Setup:**

1. Go to [railway.app](https://railway.app)
2. Click **New Project** → **Deploy from GitHub repo**
3. Select your repository
4. Add **PostgreSQL** from services
5. Set environment variables in Railway dashboard:
   - `APP_JWT_SECRET`
   - `FIREBASE_CREDENTIALS_FILE` or `FIREBASE_CREDENTIALS_BASE64`
6. Railway auto-detects Go and deploys

**Environment Variables:**
- Railway automatically sets `DATABASE_URL` when PostgreSQL is attached
- Set custom variables in **Variables** tab

---

### Option 3: Render

**Why Render:**
- Free tier available
- Managed PostgreSQL
- Auto-deploy from Git

**Setup:**

1. Go to [render.com](https://render.com)
2. Create **New Web Service** from Git
3. Set build command: `go build -o server ./cmd/server`
4. Set start command: `./server`
5. Create **PostgreSQL** database from dashboard
6. Set environment variables:
   - `DATABASE_URL` (copy from PostgreSQL service)
   - `APP_JWT_SECRET`
   - `FIREBASE_CREDENTIALS_BASE64`
7. Deploy

---

## Testing the Deployment

### 1. Health Check

```bash
curl https://your-app-url.com/health
```

Expected response: `OK`

### 2. Login Test

```bash
curl -X POST https://your-app-url.com/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "email": "driver@ropacal.com",
    "password": "driver123"
  }'
```

Expected response:
```json
{
  "token": "eyJhbGci...",
  "user": {
    "id": "...",
    "email": "driver@ropacal.com",
    "name": "John Driver",
    "role": "driver"
  }
}
```

### 3. Get Bins

```bash
curl https://your-app-url.com/api/bins
```

Should return array of 44 bins.

### 4. WebSocket Connection Test

Use a WebSocket client (wscat, Postman, or browser):

```bash
# Install wscat
npm install -g wscat

# Connect (replace with your token)
wscat -c "wss://your-app-url.com/ws?token=YOUR_JWT_TOKEN"
```

---

## Flutter App Configuration

Update your Flutter app's API endpoint:

**File:** `lib/core/config/api_config.dart` (or similar)

```dart
class ApiConfig {
  static const String baseUrl = 'https://ropacal-backend.fly.dev';
  static const String wsUrl = 'wss://ropacal-backend.fly.dev/ws';
}
```

---

## Monitoring and Logs

### Fly.io
```bash
fly logs
fly status
```

### Railway
View logs in Railway dashboard under **Deployments** → **View Logs**

### Render
View logs in Render dashboard under service **Logs** tab

---

## Troubleshooting

### Database Connection Failed

**Error:** `failed to connect to database`

**Solutions:**
1. Check `DATABASE_URL` is correctly set
2. Verify database is running
3. Check SSL mode (`sslmode=require` for production)
4. Test connection:
   ```bash
   psql $DATABASE_URL
   ```

### Firebase Initialization Failed

**Warning:** `⚠️ Failed to initialize FCM: ...`

**Solutions:**
1. Check `FIREBASE_CREDENTIALS_FILE` path is correct
2. Verify JSON file is valid
3. Ensure Firebase project has Cloud Messaging enabled
4. For production, use `FIREBASE_CREDENTIALS_BASE64`

**Note:** App will continue without push notifications.

### WebSocket Connection Refused

**Error:** Connection refused on `/ws`

**Solutions:**
1. Ensure platform supports WebSockets (Vercel does NOT)
2. Check JWT token is valid and passed in query param: `?token=...`
3. Verify CORS settings allow WebSocket origins

### Port Already in Use

**Error:** `bind: address already in use`

**Solution:**
```bash
# Find process using port 8080
lsof -i :8080

# Kill it
kill -9 <PID>

# Or change PORT in .env
PORT=3000
```

---

## Migration Checklist

- [ ] Create PostgreSQL database on chosen platform
- [ ] Set `DATABASE_URL` environment variable
- [ ] Generate and set strong `APP_JWT_SECRET`
- [ ] Upload Firebase credentials (as file or base64 env var)
- [ ] Deploy application
- [ ] Run health check test
- [ ] Test login endpoint
- [ ] Test WebSocket connection
- [ ] Update Flutter app's API endpoint
- [ ] Build Android APK with production API URL
- [ ] Test on physical device

---

## Next Steps

After successful deployment:

1. **Update CORS** to restrict origins (see Security section)
2. **Set up monitoring** (error tracking, performance monitoring)
3. **Configure backups** for PostgreSQL database
4. **Add CI/CD** for automated deployments
5. **Implement rate limiting** for API endpoints
6. **Add API documentation** (Swagger/OpenAPI)
7. **Set up staging environment** for testing

---

## Support

For issues or questions:
- Check [Go documentation](https://go.dev/doc/)
- PostgreSQL: [postgresql.org](https://postgresql.org/docs/)
- Firebase: [firebase.google.com/docs/cloud-messaging](https://firebase.google.com/docs/cloud-messaging)
- Platform-specific docs:
  - Fly.io: [fly.io/docs](https://fly.io/docs/)
  - Railway: [docs.railway.app](https://docs.railway.app/)
  - Render: [render.com/docs](https://render.com/docs/)
