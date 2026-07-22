CREATE INDEX IF NOT EXISTS idx_media_items_trash_retention
    ON media_items (status, deleted_at, id);
