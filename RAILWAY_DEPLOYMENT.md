# Deploy to Railway - Step by Step Guide

**Why Railway?** Easiest cloud deployment platform - everything through a web dashboard, no CLI needed!

---

## Step 1: Prepare Firebase Credentials (5 minutes)

### Encode Your Firebase JSON to Base64

```bash
# Navigate to your backend folder
cd /Users/omargabr/Desktop/ropacal-backend

# Encode the Firebase credentials file
cat firebase-service-account.json | base64

# Copy the output (it will be a long string)
```

**Save this base64 string** - you'll paste it into Railway in Step 4.

---

## Step 2: Create Railway Account (2 minutes)

1. Go to **[railway.app](https://railway.app)**
2. Click **"Login"**
3. Sign in with your **GitHub account**
4. Authorize Railway to access your GitHub repositories

---

## Step 3: Deploy Your Backend (3 minutes)

### Option A: Deploy from GitHub (Recommended)

1. **Push your code to GitHub first:**
   ```bash
   cd /Users/omargabr/Desktop/ropacal-backend
   git init
   git add .
   git commit -m "Initial commit - PostgreSQL backend"
   git branch -M main
   git remote add origin https://github.com/YOUR_USERNAME/ropacal-backend.git
   git push -u origin main
   ```

2. **In Railway Dashboard:**
   - Click **"New Project"**
   - Select **"Deploy from GitHub repo"**
   - Choose **`ropacal-backend`** repository
   - Railway will auto-detect Go and start building

### Option B: Deploy from Local (Alternative)

1. Install Railway CLI:
   ```bash
   brew install railway
   ```

2. Deploy:
   ```bash
   cd /Users/omargabr/Desktop/ropacal-backend
   railway login
   railway init
   railway up
   ```

---

## Step 4: Add PostgreSQL Database (1 click!)

1. In your Railway project, click **"+ New"**
2. Select **"Database"**
3. Choose **"Add PostgreSQL"**
4. Railway automatically:
   - Creates a PostgreSQL database
   - Sets `DATABASE_URL` environment variable
   - Connects it to your app ✨

**That's it!** Your app can now access PostgreSQL automatically.

---

## Step 5: Set Environment Variables (2 minutes)

1. Click on your **service** (ropacal-backend)
2. Go to **"Variables"** tab
3. Click **"+ New Variable"** and add these:

### Required Variables:

| Variable Name | Value |
|---------------|-------|
| `APP_JWT_SECRET` | Generate: `openssl rand -base64 32` (paste the output) |
| `PORT` | `8080` |
| `FIREBASE_CREDENTIALS_BASE64` | Paste the base64 string from Step 1 |

**Example:**
```
APP_JWT_SECRET=xK7mP2vQ9wR8sT1uY3zA4bC5dE6fG7hI8jK9lM0nO1pQ==
PORT=8080
FIREBASE_CREDENTIALS_BASE64=ewogICJ0eXBlIjogInNlcnZpY2VfYWNjb3VudCIsC...
```

4. Click **"Add"** for each variable

---

## Step 6: Deploy! (Automatic)

Railway will automatically:
1. Detect changes
2. Build your Go application
3. Run database migrations
4. Start the server
5. Provide you with a public URL

**Your API will be live at:**
```
https://ropacal-backend-production.up.railway.app
```

(Railway shows the URL in the "Deployments" or "Settings" tab)

---

## Step 7: Test Your Deployment (2 minutes)

### Test Health Check

```bash
curl https://your-railway-url.railway.app/health
```

Expected response: `OK`

### Test Login

```bash
curl -X POST https://your-railway-url.railway.app/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"driver@ropacal.com","password":"driver123"}'
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

### Test Get Bins

```bash
curl https://your-railway-url.railway.app/api/bins
```

Should return array of 44 bins.

---

## Step 8: Update Your Flutter App (1 minute)

Update your Flutter app to use the Railway API:

**File:** `lib/core/config/api_config.dart` (or wherever your API config is)

```dart
class ApiConfig {
  // Replace with your actual Railway URL
  static const String baseUrl = 'https://your-railway-url.railway.app';
  static const String wsUrl = 'wss://your-railway-url.railway.app/ws';
}
```

---

## Step 9: View Logs & Monitor

### View Deployment Logs

1. Go to your Railway project
2. Click on your service
3. Click **"Deployments"** tab
4. Click on the latest deployment
5. See build and runtime logs

### View Live Logs

1. Click **"Observability"** or **"Logs"** tab
2. See real-time server logs

---

## Railway Features You'll Love

### Auto-Deploy from Git
- Push to GitHub → Railway automatically deploys
- No manual deployment needed

### Environment Variables UI
- Easy to add/edit variables
- No CLI needed
- Changes trigger redeployment

### PostgreSQL Management
- Click database service → See connection details
- Built-in PostgreSQL metrics
- Easy backups

### Custom Domains (Optional)
1. Go to **Settings** → **Domains**
2. Click **"Generate Domain"** for free Railway domain
3. Or add your custom domain (e.g., `api.yourapp.com`)

---

## Troubleshooting

### Deployment Failed - Build Error

**Check build logs:**
1. Go to **Deployments** tab
2. Click failed deployment
3. Read error message

**Common fixes:**
- Ensure `go.mod` and `go.sum` are committed
- Check Go version in `go.mod` matches Railway's

### App Crashes After Deployment

**Check runtime logs:**
1. Go to **Logs** tab
2. Look for error messages

**Common issues:**

#### Missing DATABASE_URL
- Ensure PostgreSQL service is added and attached
- Check **Variables** tab for `DATABASE_URL`

#### Firebase Initialization Failed
```
⚠️ Failed to initialize FCM from base64: invalid base64
```
- Re-encode your Firebase JSON: `cat firebase-service-account.json | base64`
- Ensure no line breaks in the base64 string
- Copy the entire output

#### Invalid JWT Secret
```
APP_JWT_SECRET environment variable is required
```
- Add `APP_JWT_SECRET` in Variables tab
- Generate: `openssl rand -base64 32`

### Database Connection Issues

**Error:** `failed to connect to database`

**Solutions:**
1. Check PostgreSQL service is running (green status)
2. Verify `DATABASE_URL` is set automatically
3. In **Database** → **Settings**, check connection string format

### WebSocket Connection Refused

**Verify WebSocket support:**
```bash
wscat -c "wss://your-railway-url.railway.app/ws?token=YOUR_JWT_TOKEN"
```

Railway fully supports WebSockets - no special configuration needed!

---

## Cost & Free Tier

**Railway Free Tier:**
- $5 of usage per month (free credit)
- Enough for:
  - Small PostgreSQL database
  - 1 backend service
  - Low-medium traffic testing

**Typical usage for your app:**
- Backend service: ~$2-3/month
- PostgreSQL: ~$2-3/month
- **Total: ~$4-6/month** (covered by free credit for testing)

**Upgrading:**
- Pay-as-you-go after free credit
- No subscription required
- Only pay for what you use

---

## Next Steps After Deployment

1. **Test on Android Device:**
   - Build Android APK with production API URL
   - Install on phone
   - Test with real GPS and navigation

2. **Monitor Usage:**
   - Check Railway dashboard for resource usage
   - Monitor logs for errors

3. **Set Up Custom Domain (Optional):**
   - Purchase domain (e.g., `api.ropacal.com`)
   - Add to Railway settings

4. **Production Hardening:**
   - Update CORS to restrict origins (see DEPLOYMENT.md)
   - Set up error monitoring (Sentry, etc.)
   - Configure database backups

---

## Summary - What Just Happened?

✅ Your Go backend is running on Railway's servers
✅ PostgreSQL database is live and connected
✅ Firebase push notifications configured
✅ WebSocket real-time updates working
✅ Public API URL available for your Flutter app
✅ Auto-deploys on every Git push

**Total time: ~15 minutes** from zero to deployed! 🚀

---

## Getting Help

**Railway Discord:**
- Join: [discord.gg/railway](https://discord.gg/railway)
- Very active community
- Railway team responds quickly

**Railway Docs:**
- [docs.railway.app](https://docs.railway.app/)
- PostgreSQL guides
- Go deployment guides

**Your Backend Logs:**
- Always check Railway logs first
- Most issues are visible in deployment/runtime logs
