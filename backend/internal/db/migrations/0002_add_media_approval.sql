-- Existing media remains visible. New application code inserts NULL until an
-- admin approves it when moderation is enabled.
ALTER TABLE media_items ADD COLUMN approved_at TEXT;
UPDATE media_items SET approved_at = uploaded_at;

CREATE INDEX IF NOT EXISTS idx_media_items_visibility_uploaded_at
    ON media_items (status, approved_at, uploaded_at, id);
