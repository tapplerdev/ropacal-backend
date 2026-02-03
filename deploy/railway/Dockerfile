# Railway OSRM Deployment - Dockerfile
# Deploys OSRM Map Matching service with California OSM data

FROM ghcr.io/project-osrm/osrm-backend:latest

# Install dependencies for downloading OSM data (Alpine Linux uses apk)
RUN apk add --no-cache \
    curl \
    wget

# Set working directory
WORKDIR /data

# Download and process California OSM data
# This happens at build time, so data is baked into the image
RUN echo "📥 Downloading California OSM data (~500MB)..." && \
    wget -q --show-progress https://download.geofabrik.de/north-america/us/california-latest.osm.pbf && \
    echo "✅ Download complete" && \
    \
    echo "🔧 Extracting road network..." && \
    osrm-extract -p /opt/car.lua /data/california-latest.osm.pbf && \
    echo "✅ Extraction complete" && \
    \
    echo "📊 Creating routing graph..." && \
    osrm-partition /data/california-latest.osrm && \
    echo "✅ Partition complete" && \
    \
    echo "⚡ Optimizing routing graph..." && \
    osrm-customize /data/california-latest.osrm && \
    echo "✅ Optimization complete" && \
    \
    echo "🧹 Cleaning up raw OSM file..." && \
    rm -f california-latest.osm.pbf && \
    echo "✅ Cleanup complete"

# Railway injects PORT environment variable at runtime
# We need to expose it dynamically
ARG PORT=5000
ENV PORT=$PORT

# EXPOSE is just documentation, Railway will map whatever port the app listens on
EXPOSE $PORT

# Start OSRM server
# IMPORTANT: Use shell form (not JSON array) so $PORT expands correctly
# Railway requires listening on 0.0.0.0 (not localhost)
CMD osrm-routed \
    --algorithm mld \
    --max-matching-size 5000 \
    --ip 0.0.0.0 \
    --port ${PORT:-5000} \
    /data/california-latest.osrm

# Image size: ~2.5GB (OSM data + OSRM processing output)
# RAM usage: ~1-2GB at runtime
# CPU: Low (< 10% for typical usage)
