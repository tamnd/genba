-- document_own is a person saying the owner the source reported is wrong.
--
-- Ownership is derived, and derived ownership is wrong often enough that a
-- corpus which cannot be corrected slowly stops being trusted. The connector
-- reports whoever the source calls the owner, which is the account that ran the
-- import or the person who created the folder in 2019, and neither of them will
-- answer a question about the document or re-verify it.
--
-- The corrected owner is written into the document as well as into this table,
-- and that is deliberate. The columns on document are what an owner: filter and
-- a facet count are computed from, so a correction that lived only here would
-- leave the interface showing one person and the search matching another. No
-- query path reads this table. What it is for is making the correction outlive
-- the next crawl and making it possible to undo.
--
-- The three people are stored as JSON rather than as nine columns, because they
-- go back into the document when the correction is cleared and a person
-- flattened into a subject, a name and an address comes back missing the source
-- identity that an owner: filter matches on. It is text rather than jsonb for
-- the same reason document_data is: nothing queries into it, and jsonb would
-- parse it on the way in and rebuild it on the way out to store the same bytes.
--
-- The cascade is what keeps it honest, exactly as it is for document_verify. A
-- document that leaves the corpus takes its correction with it, so re-crawling
-- a document that was deleted brings back the document without handing it to
-- somebody named in a correction about the old one.
CREATE TABLE document_own (
    doc_id       text   PRIMARY KEY REFERENCES document(id) ON DELETE CASCADE,

    -- Who owns it, and what the source said before anybody disagreed.
    --
    -- was is refreshed on every write of the document, so clearing a correction
    -- puts back the answer the connector gives today rather than the one it
    -- gave the day somebody first corrected it. A source that fixes its own
    -- metadata is a source whose answer is worth going back to.
    owner        text   NOT NULL,
    was          text   NOT NULL,

    -- Who said so and when. A change of ownership with no name against it is a
    -- change nobody can query, and the person shown in the interface is the
    -- person who has to be asked when the correction is itself wrong.
    --
    -- Unix nanoseconds, for the same reason document_verify.verified_at is: a
    -- timestamptz keeps microseconds, and a time read back out of one does not
    -- compare equal to the time that went in.
    corrected_by text   NOT NULL,
    corrected_at bigint NOT NULL
);
