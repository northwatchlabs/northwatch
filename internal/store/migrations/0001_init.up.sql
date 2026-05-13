CREATE TABLE components (
  kind         TEXT    NOT NULL CHECK (kind      NOT LIKE '%/%'),
  namespace    TEXT    NOT NULL CHECK (namespace NOT LIKE '%/%'),
  name         TEXT    NOT NULL CHECK (name      NOT LIKE '%/%'),
  id           TEXT    GENERATED ALWAYS AS (kind || '/' || namespace || '/' || name) STORED
               UNIQUE,
  display_name TEXT    NOT NULL DEFAULT '',
  status       TEXT    NOT NULL DEFAULT 'unknown'
               CHECK (status IN ('unknown','operational','degraded','down')),
  updated_at   INTEGER NOT NULL
) STRICT;

CREATE TABLE incidents (
  id           TEXT    PRIMARY KEY,
  component_id TEXT    NOT NULL
               REFERENCES components(id) ON DELETE CASCADE,
  title        TEXT    NOT NULL,
  status       TEXT    NOT NULL DEFAULT 'investigating'
               CHECK (status IN ('investigating','identified','monitoring','resolved')),
  opened_at    INTEGER NOT NULL,
  resolved_at  INTEGER
) STRICT;
CREATE INDEX idx_incidents_component_open
  ON incidents(component_id) WHERE resolved_at IS NULL;

CREATE TABLE incident_updates (
  id          TEXT    PRIMARY KEY,
  incident_id TEXT    NOT NULL
              REFERENCES incidents(id) ON DELETE CASCADE,
  body        TEXT    NOT NULL,
  status      TEXT    NOT NULL
              CHECK (status IN ('investigating','identified','monitoring','resolved')),
  created_at  INTEGER NOT NULL
) STRICT;
CREATE INDEX idx_incident_updates_incident
  ON incident_updates(incident_id, created_at);

CREATE TABLE status_history (
  component_id     TEXT    NOT NULL
                   REFERENCES components(id) ON DELETE CASCADE,
  day              INTEGER NOT NULL,
  status           TEXT    NOT NULL
                   CHECK (status IN ('unknown','operational','degraded','down')),
  downtime_seconds INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (component_id, day)
) STRICT;
