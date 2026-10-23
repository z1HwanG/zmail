-- zMail SQLite schema. 由 store.Open 在启动时执行，全部为幂等语句。
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS accounts (
  id             TEXT PRIMARY KEY,
  provider       TEXT NOT NULL DEFAULT 'imap',
  email          TEXT NOT NULL UNIQUE,
  display_name   TEXT NOT NULL DEFAULT '',
  username       TEXT NOT NULL,
  password_enc   BLOB NOT NULL,
  imap_host      TEXT NOT NULL,
  imap_port      INTEGER NOT NULL DEFAULT 993,
  imap_secure    INTEGER NOT NULL DEFAULT 1,
  smtp_host      TEXT NOT NULL,
  smtp_port      INTEGER NOT NULL DEFAULT 465,
  smtp_secure    INTEGER NOT NULL DEFAULT 1,
  status         TEXT NOT NULL DEFAULT 'active',
  last_error     TEXT NOT NULL DEFAULT '',
  last_synced_at TEXT,
  created_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS folders (
  id             TEXT PRIMARY KEY,
  account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  remote_path    TEXT NOT NULL,
  name           TEXT NOT NULL,
  role           TEXT NOT NULL DEFAULT 'custom',
  uid_validity   INTEGER NOT NULL DEFAULT 0,
  last_uid       INTEGER NOT NULL DEFAULT 0,
  unread_count   INTEGER NOT NULL DEFAULT 0,
  total_count    INTEGER NOT NULL DEFAULT 0,
  enabled        INTEGER NOT NULL DEFAULT 1,
  priority       INTEGER NOT NULL DEFAULT 100,
  last_synced_at TEXT,
  backfill_uid   INTEGER NOT NULL DEFAULT 0,
  UNIQUE(account_id, remote_path)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_folders_account_role
  ON folders(account_id, role) WHERE role = 'inbox';

CREATE TABLE IF NOT EXISTS threads (
  id              TEXT PRIMARY KEY,
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  thread_key      TEXT NOT NULL,
  subject         TEXT NOT NULL DEFAULT '',
  last_message_at TEXT NOT NULL,
  message_count   INTEGER NOT NULL DEFAULT 1,
  UNIQUE(account_id, thread_key)
);

CREATE TABLE IF NOT EXISTS messages (
  id              TEXT PRIMARY KEY,
  account_id      TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  folder_id       TEXT NOT NULL REFERENCES folders(id) ON DELETE CASCADE,
  thread_id       TEXT REFERENCES threads(id) ON DELETE SET NULL,
  remote_uid      INTEGER NOT NULL,
  message_id      TEXT,
  subject         TEXT NOT NULL DEFAULT '',
  from_name       TEXT NOT NULL DEFAULT '',
  from_email      TEXT NOT NULL DEFAULT '',
  to_json         TEXT NOT NULL DEFAULT '[]',
  cc_json         TEXT NOT NULL DEFAULT '[]',
  bcc_json        TEXT NOT NULL DEFAULT '[]',
  sent_at         TEXT,
  received_at     TEXT NOT NULL,
  snippet         TEXT NOT NULL DEFAULT '',
  body_text       TEXT NOT NULL DEFAULT '',
  body_html       TEXT NOT NULL DEFAULT '',
  has_attachments INTEGER NOT NULL DEFAULT 0,
  is_read         INTEGER NOT NULL DEFAULT 0,
  is_starred      INTEGER NOT NULL DEFAULT 0,
  is_deleted      INTEGER NOT NULL DEFAULT 0,
  is_draft        INTEGER NOT NULL DEFAULT 0,
  size_bytes      INTEGER NOT NULL DEFAULT 0,
  raw_key         TEXT,
  created_at      TEXT NOT NULL,
  UNIQUE(account_id, folder_id, remote_uid)
);

CREATE INDEX IF NOT EXISTS idx_messages_folder
  ON messages(folder_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_account
  ON messages(account_id, received_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_message_id
  ON messages(account_id, message_id);
CREATE INDEX IF NOT EXISTS idx_messages_thread
  ON messages(thread_id, received_at DESC);

CREATE TABLE IF NOT EXISTS attachments (
  id          TEXT PRIMARY KEY,
  message_id  TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  filename    TEXT NOT NULL,
  mime_type   TEXT NOT NULL,
  size        INTEGER NOT NULL DEFAULT 0,
  content_id  TEXT,
  sha256      TEXT NOT NULL DEFAULT '',
  storage_key TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachments_sha
  ON attachments(sha256);
CREATE INDEX IF NOT EXISTS idx_attachments_message
  ON attachments(message_id);

CREATE TABLE IF NOT EXISTS sync_logs (
  id         TEXT PRIMARY KEY,
  account_id TEXT NOT NULL,
  level      TEXT NOT NULL,
  message    TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sync_logs_account
  ON sync_logs(account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sync_logs_created
  ON sync_logs(created_at);

CREATE TABLE IF NOT EXISTS jobs (
  id         TEXT PRIMARY KEY,
  kind       TEXT NOT NULL,
  account_id TEXT,
  payload    TEXT NOT NULL DEFAULT '{}',
  run_at     TEXT NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL DEFAULT 'pending',
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_due
  ON jobs(status, run_at);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  subject,
  body_text,
  from_name,
  from_email,
  content='messages',
  content_rowid='rowid',
  tokenize='trigram'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, subject, body_text, from_name, from_email)
  VALUES (new.rowid, new.subject, new.body_text, new.from_name, new.from_email);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body_text, from_name, from_email)
  VALUES ('delete', old.rowid, old.subject, old.body_text, old.from_name, old.from_email);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, body_text, from_name, from_email)
  VALUES ('delete', old.rowid, old.subject, old.body_text, old.from_name, old.from_email);
  INSERT INTO messages_fts(rowid, subject, body_text, from_name, from_email)
  VALUES (new.rowid, new.subject, new.body_text, new.from_name, new.from_email);
END;
