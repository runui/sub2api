-- Enable explicitly per group; a saved price does not imply permission.
ALTER TABLE groups ADD COLUMN IF NOT EXISTS allow_video_generation boolean NOT NULL DEFAULT false;
