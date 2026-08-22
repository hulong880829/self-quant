CREATE TABLE IF NOT EXISTS accounts (
    username TEXT PRIMARY KEY,
    password_hash TEXT NOT NULL,
    permission TEXT NOT NULL DEFAULT 'user',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seed admin / admin123 (bcrypt). Password hash only; plaintext is never stored.
INSERT INTO accounts (username, password_hash, permission)
VALUES (
    'admin',
    '$2a$10$tc0y6ott0.7Cy57nvF7UpO1WP7XtEEMNusRos3UHOcDulVlDuXg66',
    'admin'
)
ON CONFLICT (username) DO NOTHING;
