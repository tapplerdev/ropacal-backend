#!/bin/bash

# One-time script to load real bin data into production database
# Run this once to replace test data with real Bay Area bins

echo "🔄 Loading real bin data..."
echo ""
echo "This will:"
echo "  1. Delete all non-Dallas bins (test data)"
echo "  2. Insert 44 real Bay Area bins"
echo ""
read -p "Continue? (y/n) " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]
then
    echo "Cancelled."
    exit 1
fi

# Check if DATABASE_URL is set
if [ -z "$DATABASE_URL" ]; then
    echo "❌ ERROR: DATABASE_URL environment variable not set"
    echo "Please set it to your Railway PostgreSQL URL"
    exit 1
fi

# Execute the migration SQL file
echo "📤 Executing migration..."
psql "$DATABASE_URL" -f migrations/load_real_bins.sql

if [ $? -eq 0 ]; then
    echo ""
    echo "✅ Migration completed successfully!"
else
    echo ""
    echo "❌ Migration failed"
    exit 1
fi
