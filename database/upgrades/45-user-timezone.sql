-- v45: Add timezone column for users

ALTER TABLE "user" ADD COLUMN timezone TEXT;
