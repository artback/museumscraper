#!/bin/sh
# Dumps the catalogue on a schedule and keeps the most recent few.
#
# Backups that depend on someone remembering to run them are not backups. This
# runs as an ordinary service so the protection is on by default, and prunes old
# dumps so it cannot fill the disk it is protecting.
#
# Configured by environment: BACKUP_INTERVAL_HOURS, BACKUP_KEEP, PGHOST,
# PGUSER, PGDATABASE.
set -eu

INTERVAL_HOURS="${BACKUP_INTERVAL_HOURS:-24}"
KEEP="${BACKUP_KEEP:-7}"
DIR=/backups

# take writes one dump, to a temporary name first so a dump interrupted midway
# is never mistaken for a complete one. Only a finished file gets the real name.
take() {
    stamp=$(date +%Y%m%d-%H%M%S)
    partial="$DIR/.museum-$stamp.dump.partial"
    final="$DIR/museum-$stamp.dump"

    if pg_dump --format=custom --compress=9 -f "$partial" 2>/tmp/dump.err; then
        mv "$partial" "$final"
        echo "$(date -u +%FT%TZ) wrote $(basename "$final") ($(du -h "$final" | cut -f1))"
    else
        rm -f "$partial"
        echo "$(date -u +%FT%TZ) backup FAILED: $(tail -1 /tmp/dump.err)" >&2
        return 1
    fi
}

# prune keeps the newest $KEEP dumps. Without it the thing protecting the disk
# is what eventually fills it.
prune() {
    count=$(find "$DIR" -maxdepth 1 -name 'museum-*.dump' | wc -l)
    if [ "$count" -le "$KEEP" ]; then
        return 0
    fi
    find "$DIR" -maxdepth 1 -name 'museum-*.dump' | sort | head -n "$((count - KEEP))" |
        while read -r old; do
            rm -f "$old"
            echo "$(date -u +%FT%TZ) pruned $(basename "$old")"
        done
}

# Clean up anything a previous container left half-written.
rm -f "$DIR"/.museum-*.dump.partial 2>/dev/null || true

echo "$(date -u +%FT%TZ) backups every ${INTERVAL_HOURS}h, keeping ${KEEP}"

while true; do
    # A failed dump must not stop the loop: the database may simply be busy or
    # restarting, and the next attempt should still happen.
    take || true
    prune || true
    sleep "$((INTERVAL_HOURS * 3600))"
done
