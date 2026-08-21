-- document_open is what each person opened, which is the half of the recent
-- screen that no amount of searching can reconstruct.
--
-- One row per person per document rather than one per open, because the list
-- answers what somebody was reading, and a person who opened the same runbook
-- nine times this morning wants the other nineteen entries rather than nine
-- copies of that one.
--
-- The cascade is what keeps it honest. A document that leaves the corpus leaves
-- everybody's history with it, so this table cannot hold an id that nothing
-- else in the database knows about, and nobody has to write a sweep for it.
CREATE TABLE document_open (
    tenant    text   NOT NULL,
    subject   text   NOT NULL,
    doc_id    text   NOT NULL REFERENCES document(id) ON DELETE CASCADE,

    -- Unix nanoseconds, for the same reason document.modified_at is: a
    -- timestamptz keeps microseconds, and a time read back out of one does not
    -- compare equal to the time that went in.
    opened_at bigint NOT NULL,

    PRIMARY KEY (tenant, subject, doc_id)
);

-- The read is one person's history, newest first, and this is the whole of it:
-- an index only scan of at most twenty rows, with no sort.
CREATE INDEX document_open_recent ON document_open (tenant, subject, opened_at DESC);
