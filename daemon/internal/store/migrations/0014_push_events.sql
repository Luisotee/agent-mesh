-- What a device chose to be pushed: a comma-separated list of notice kinds (app.notify.events' choices), empty to
-- follow the host's own list. Set by each RegisterPush, so a re-registration replaces it.
ALTER TABLE push_endpoints ADD COLUMN events TEXT NOT NULL DEFAULT '';
