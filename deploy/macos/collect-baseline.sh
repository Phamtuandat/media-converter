#!/bin/sh
set -eu

if [ "$#" -ne 5 ]; then
    echo "usage: $0 <service-base-url> <old-binary> <old-plist> <legacy-artifact-root> <report>" >&2
    exit 2
fi

service_url=$1
old_binary=$2
old_plist=$3
legacy_root=$4
report=$5

mkdir -p "$(dirname "$report")"
{
    echo "timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "service_url=$service_url"
    echo "old_binary=$old_binary"
    echo "old_binary_sha256=$(shasum -a 256 "$old_binary" | awk '{print $1}')"
    echo "old_plist=$old_plist"
    echo "old_plist_sha256=$(shasum -a 256 "$old_plist" | awk '{print $1}')"
    echo "legacy_artifact_root=$legacy_root"
    echo "legacy_artifact_count=$(find "$legacy_root" -type f -print | wc -l | tr -d ' ')"
    echo "legacy_artifact_bytes=$(find "$legacy_root" -type f -exec stat -f '%z' {} + | awk '{sum += $1} END {print sum + 0}')"
    echo "health_live=$(curl -sS "$service_url/health/live" || true)"
    echo "health_ready=$(curl -sS "$service_url/health/ready" || true)"
    echo "hermes_smoke=record separately; this script never prints or reads MEDIA_SERVICE_TOKEN"
} >"$report"

chmod 600 "$report"
echo "baseline written to $report"
