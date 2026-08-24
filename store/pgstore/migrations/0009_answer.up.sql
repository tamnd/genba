-- answer is a question somebody answered, and it is the first table here that
-- is not about a document.
--
-- So it is the first one keyed by tenant. Everything else reaches its tenant
-- through the document it hangs off, and an answer hangs off nothing: it is the
-- product's own text rather than a fact about a file, which is also why it has
-- no cascade to be deleted by.
CREATE TABLE answer (
    id         text   NOT NULL,
    tenant     text   NOT NULL,

    -- The canonical phrasing and the other ways people ask it, both kept
    -- verbatim because they are read by a person maintaining the answer. What a
    -- lookup matches against lives in answer_phrasing below.
    question   text   NOT NULL,
    variants   text   NOT NULL,

    -- The answer itself, as the markdown somebody typed. Not bounded here: an
    -- answer long enough to be a problem is a document, and whoever wrote it
    -- finds that out from the reader who has to scroll past it.
    body       text   NOT NULL,

    -- Document ids as JSON rather than a join table, because nothing queries
    -- into them. They are resolved one page at a time through whoever is
    -- reading, which is what stops an answer from listing the documents a reader
    -- may not open. Text rather than jsonb for the same reason document_own
    -- stores its people as text.
    sources    text   NOT NULL,

    -- Who wrote it, carried whole, so the card has a name on it without a
    -- second lookup.
    author     text   NOT NULL,

    -- Unix nanoseconds, like every other time in this schema, because a
    -- timestamptz keeps microseconds and a time read back out of one does not
    -- compare equal to the time that went in. until is stored rather than
    -- derived from the cadence, so an answer written under an older policy keeps
    -- the deadline it was given.
    written_at bigint NOT NULL,
    until      bigint NOT NULL,

    PRIMARY KEY (tenant, id)
);

-- The list of answers is read most recently written first by whoever maintains
-- them, and it is the only query over this table that is not a point lookup.
CREATE INDEX answer_recent ON answer (tenant, written_at DESC);

-- answer_phrasing is every way an answer can be asked for, folded by
-- store.AnswerKey, one row per phrasing.
--
-- A table rather than a JSON column on the answer, because this is the one
-- thing about an answer that is on the search path: every search asks whether
-- anybody has written this question down, and that question has to be a single
-- primary key probe or the card costs more than the results it sits above.
--
-- The key is unique within the tenant, so a phrasing claimed by a second answer
-- moves to it. Two answers claiming the same question is a curation conflict
-- rather than a storage problem, and the resolution that needs no screen is that
-- the most recent writer wins.
CREATE TABLE answer_phrasing (
    tenant text NOT NULL,
    key    text NOT NULL,
    id     text NOT NULL,

    PRIMARY KEY (tenant, key),
    FOREIGN KEY (tenant, id) REFERENCES answer (tenant, id) ON DELETE CASCADE
);

-- Retracting an answer and editing one both have to find the rows pointing at
-- it, and without this that is a scan of every phrasing in the tenant. It is
-- also what the foreign key above needs to check without one.
CREATE INDEX answer_phrasing_answer ON answer_phrasing (tenant, id);
