-- document_verify is who vouched for a document and until when.
--
-- One row per document rather than a history, because what a reader needs is
-- the current claim, and reconstructing it from a log would mean reading every
-- line ever written about the document to draw one badge. Who verified what and
-- when is the audit log's question, and it is a different table with a
-- different retention.
--
-- The verifier is denormalised into three columns rather than referencing a
-- person, because there is no person table here: a subject is whatever the
-- identity provider called them, and the name on the badge is the name at the
-- moment the claim was made. A claim that silently changes whose name is on it
-- when somebody's profile is updated is not the same claim.
--
-- The cascade is what keeps it honest. A document that leaves the corpus takes
-- its verification with it, so this table cannot hold an id that nothing else
-- in the database knows about, and re-crawling a document that was deleted
-- brings back the document without bringing back a claim nobody made about the
-- new version. A document that is merely rewritten by a crawl keeps its row,
-- because a write is an upsert on the same id rather than a delete and an
-- insert.
CREATE TABLE document_verify (
    doc_id      text   PRIMARY KEY REFERENCES document(id) ON DELETE CASCADE,

    -- Who made the claim, carried whole so that a badge has a name on it
    -- without a second lookup. A badge that says u-4181 vouched for this is
    -- worse than no badge, because the point of the signal is that a reader
    -- recognises the name.
    by_subject  text   NOT NULL,
    by_name     text   NOT NULL,
    by_email    text   NOT NULL,

    -- Unix nanoseconds, for the same reason document.modified_at is: a
    -- timestamptz keeps microseconds, and a time read back out of one does not
    -- compare equal to the time that went in.
    --
    -- expires_at is stored rather than derived from verified_at plus a cadence,
    -- because the cadence is a policy that will change and a document verified
    -- under the old one should keep the expiry it was given.
    verified_at bigint NOT NULL,
    expires_at  bigint NOT NULL,

    -- Why, in the verifier's own words, and usually empty. It is here for the
    -- case the badge cannot express: verified except for the section about the
    -- old cluster, which is the sentence that saves the next reader an hour.
    note        text   NOT NULL
);

-- The queue of what is about to go stale is a walk down this index, and it is
-- the only read of this table that is not keyed by document.
CREATE INDEX document_verify_expiry ON document_verify (expires_at);
