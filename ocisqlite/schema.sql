-- Schema version 2. Content lives in blobs/<algorithm>/<encoded digest>.
CREATE TABLE repository (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL
);
CREATE TABLE blobs (
    digest TEXT PRIMARY KEY,
    size INTEGER NOT NULL CHECK (size >= 0),
    created_at INTEGER NOT NULL
);
CREATE TABLE repository_blob (
    repository_id INTEGER NOT NULL REFERENCES repository(id) ON DELETE CASCADE,
    digest TEXT NOT NULL REFERENCES blobs(digest),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (repository_id, digest)
);
CREATE INDEX repository_blob_digest ON repository_blob(digest);
CREATE TABLE manifests (
    repository_id INTEGER NOT NULL,
    digest TEXT NOT NULL,
    media_type TEXT NOT NULL,
    artifact_type TEXT NOT NULL,
    subject TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (repository_id, digest),
    FOREIGN KEY (repository_id, digest) REFERENCES repository_blob(repository_id, digest)
);
-- Subjects may be dangling, as required by the distribution specification.
CREATE INDEX manifests_subject ON manifests(repository_id, subject, digest);
CREATE TABLE manifest_blob (
    repository_id INTEGER NOT NULL,
    manifest TEXT NOT NULL,
    blob_digest TEXT NOT NULL,
    PRIMARY KEY (repository_id, manifest, blob_digest),
    FOREIGN KEY (repository_id, manifest) REFERENCES manifests(repository_id, digest) ON DELETE CASCADE
);
CREATE INDEX manifest_blob_digest ON manifest_blob(repository_id, blob_digest);
CREATE TABLE manifest_manifest (
    repository_id INTEGER NOT NULL,
    manifest TEXT NOT NULL,
    child_digest TEXT NOT NULL,
    PRIMARY KEY (repository_id, manifest, child_digest),
    FOREIGN KEY (repository_id, manifest) REFERENCES manifests(repository_id, digest) ON DELETE CASCADE
);
CREATE INDEX manifest_manifest_child ON manifest_manifest(repository_id, child_digest);
CREATE TABLE manifest_annotations (
    repository_id INTEGER NOT NULL,
    manifest TEXT NOT NULL,
    annotation_key TEXT NOT NULL,
    annotation_value TEXT NOT NULL,
    PRIMARY KEY (repository_id, manifest, annotation_key),
    FOREIGN KEY (repository_id, manifest) REFERENCES manifests(repository_id, digest) ON DELETE CASCADE
);
CREATE TABLE tag (
    repository_id INTEGER NOT NULL,
    name TEXT NOT NULL,
    digest TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (repository_id, name),
    FOREIGN KEY (repository_id, digest) REFERENCES manifests(repository_id, digest) ON DELETE CASCADE
);
CREATE INDEX tag_digest ON tag(repository_id, digest);
CREATE TABLE tag_history (
    id INTEGER PRIMARY KEY,
    repository_id INTEGER NOT NULL REFERENCES repository(id) ON DELETE CASCADE,
    tag TEXT NOT NULL,
    digest TEXT NOT NULL,
    media_type TEXT NOT NULL,
    artifact_type TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    event_type TEXT NOT NULL CHECK (event_type IN ('created', 'deleted')),
    created_at INTEGER NOT NULL
);
CREATE INDEX tag_history_time ON tag_history(repository_id, tag, created_at DESC);
CREATE INDEX tag_history_digest ON tag_history(repository_id, tag, digest, created_at DESC);
CREATE TABLE upload (
    session TEXT PRIMARY KEY,
    repository_id INTEGER NOT NULL REFERENCES repository(id) ON DELETE CASCADE,
    size INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    created_at INTEGER NOT NULL,
    last_upload_at INTEGER NOT NULL
);
CREATE INDEX upload_repository ON upload(repository_id);
PRAGMA user_version = 2;
