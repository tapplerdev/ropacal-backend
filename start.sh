#!/bin/bash

echo "🚀 Starting Ropacal Backend Server..."
echo ""

# Check if Firebase credentials exist
if [ ! -f "firebase-service-account.json" ]; then
    echo "⚠️  Warning: firebase-service-account.json not found"
    echo "   Push notifications will be disabled"
    echo ""
fi

# Build the server
echo "📦 Building server..."
go build -o bin/server ./cmd/server

if [ $? -ne 0 ]; then
    echo "❌ Build failed"
    exit 1
fi

echo "✅ Build successful"
echo ""

# Run the server
echo "🌐 Starting server on http://localhost:8080"
echo "   Press Ctrl+C to stop"
echo ""

./bin/server
