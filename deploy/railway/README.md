# Railway OSRM Deployment

Deploy OSRM Map Matching service to Railway in 3 steps:

## Quick Start

### 1. Push to Railway

**Via Dashboard** (easiest):
1. Go to https://railway.com/new
2. Select "Deploy from GitHub repo"
3. Connect your `ropacal-backend` repo
4. Set Dockerfile path: `deploy/railway/Dockerfile`
5. Click "Deploy"

**Via CLI**:
```bash
npm i -g @railway/cli
railway login
railway init
railway up
```

### 2. Get Public URL

Railway will generate a URL like:
```
https://osrm-routing-production-xxxx.up.railway.app
```

### 3. Update Backend

```bash
# .env
SNAP_PROVIDER=osrm
OSRM_SERVER_URL=https://YOUR_RAILWAY_URL
```

## What's Included

- ✅ **Dockerfile**: Production-ready OSRM container
- ✅ **California OSM Data**: Pre-loaded and processed (~500MB)
- ✅ **Auto-restart**: Railway handles failures
- ✅ **Port Configuration**: Automatic via Railway's `$PORT` variable

## Cost

**~$10-12/month** (vs $162/month Google Roads API)

- CPU: ~$5/month
- RAM (1-2GB): ~$5/month
- Network: ~$1/month

## Files

```
deploy/railway/
├── Dockerfile              # Main deployment config
├── railway.json            # Railway-specific settings (optional)
├── .dockerignore          # Exclude unnecessary files
├── README.md              # This file
└── RAILWAY_DEPLOYMENT_GUIDE.md  # Complete step-by-step guide
```

## Testing Locally First?

```bash
# Build and run locally (takes ~10 minutes)
docker build -t osrm-test .
docker run -p 5000:5000 osrm-test

# Test
curl 'http://localhost:5000/match/v1/driving/-121.886329,37.335480;-121.886400,37.335600?radiuses=10;8'
```

## Full Documentation

See **[RAILWAY_DEPLOYMENT_GUIDE.md](./RAILWAY_DEPLOYMENT_GUIDE.md)** for complete instructions including:
- Detailed deployment steps
- Cost breakdown
- Monitoring setup
- Troubleshooting
- Security best practices
- Monthly OSM data updates

## Support

- Railway: https://docs.railway.com
- OSRM: https://project-osrm.org/docs
