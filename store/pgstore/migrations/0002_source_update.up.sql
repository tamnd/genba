-- source_update is the source's own revision for a document, which is what a
-- reconciliation compares against the revision the source reports now.
--
-- It is a column rather than a field read out of the stored JSON because a
-- reconciliation walks every document of a source, and decoding a four kilobyte
-- document to reach a forty byte string would turn a scan of an index into a
-- read of the whole corpus. That is the same argument document_data was split
-- out on.
ALTER TABLE document ADD COLUMN source_update text NOT NULL DEFAULT '';

-- The cast is because data is stored as text rather than as jsonb, which is
-- the choice document_data made on purpose: the column is handed back to Get
-- byte for byte and nothing queries inside it. This backfill is the one place
-- that has to look, and it runs once.
UPDATE document d SET source_update = COALESCE(x.data::jsonb ->> 'SourceUpdate', '')
FROM document_data x WHERE x.doc_id = d.id;

-- The reconciliation predicate. document_source already exists and covers the
-- source alone, which is the wrong way round for a walk that is always scoped
-- to one tenant first.
CREATE INDEX document_tenant_source ON document (tenant, source);
