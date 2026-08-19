-- The document row: everything a query filters, counts or ranks on, and nothing
-- else. The document itself lives in document_data, and the reason is in the
-- comment on that table.
CREATE TABLE document (
    id             text     PRIMARY KEY,
    tenant         text     NOT NULL,
    source         text     NOT NULL,
    kind           text     NOT NULL,

    -- container_fold, author_keys and owner_keys are folded copies of what a
    -- text filter compares against, computed by store.Fold and
    -- store.PersonKeys so that the comparison in SQL is the comparison in Go
    -- rather than the database's idea of lower case.
    container_fold text     NOT NULL,
    author_keys    text[]   NOT NULL,
    owner_keys     text[]   NOT NULL,

    -- modified_at is unix nanoseconds rather than a timestamptz, because
    -- timestamptz keeps microseconds and a document read back with its
    -- nanoseconds rounded off is not the document that was written. The column
    -- below gives an operator the readable form at no storage cost.
    modified_at    bigint,
    modified_utc   timestamptz GENERATED ALWAYS AS (to_timestamp(modified_at / 1000000000.0)) VIRTUAL,

    -- mode and owner_key are the parts of the permission descriptor the
    -- visibility predicate needs in a column. The rest lives in document_ref.
    mode           smallint NOT NULL,
    owner_key      text     NOT NULL,

    -- queryable is doc.Document.Queryable. A row with a false here is
    -- quarantined and no query path may return it.
    queryable      boolean  NOT NULL,

    -- Token counts, not a weighted length. The weight a ranker puts on a title
    -- is part of the ranking function, and a column that had it baked in would
    -- be a second place that function lives.
    title_tokens   integer  NOT NULL DEFAULT 0,
    body_tokens    integer  NOT NULL DEFAULT 0,

    -- The display forms of the two fields that are faceted on. container_fold
    -- and author_keys are already here for filtering, and a facet needs the
    -- string a person reads.
    container      text     NOT NULL DEFAULT '',
    author_name    text     NOT NULL DEFAULT '',

    -- terms is the full text index, and it is built in Go rather than by
    -- to_tsvector. The lexemes are exactly the terms doc.Tokenize produced, so
    -- the match set this column answers is the match set store.Request.Matches
    -- defines, with no second tokenizer in the middle to disagree with the
    -- first. The positions are synthesised from the per field occurrence
    -- counts, which is what gives ts_rank a real frequency signal to order the
    -- candidate cut by, and the A and B weights carry the title and the body.
    terms          tsvector NOT NULL DEFAULT ''::tsvector
);

CREATE INDEX document_tenant ON document (tenant, queryable);
CREATE INDEX document_source ON document (source);
CREATE INDEX document_modified ON document (modified_at);
CREATE INDEX document_container ON document (container_fold);
CREATE INDEX document_terms ON document USING gin (terms);
CREATE INDEX document_author_keys ON document USING gin (author_keys);
CREATE INDEX document_owner_keys ON document USING gin (owner_keys);

-- document_ref is the allow and deny lists, one row per reference, in the key
-- form acl.Ref produces. The permission predicate is set membership against
-- this table, which is the same set membership acl.Permissions performs in Go.
CREATE TABLE document_ref (
    doc_id text     NOT NULL REFERENCES document (id) ON DELETE CASCADE,
    effect smallint NOT NULL, -- 0 allow, 1 deny
    scope  smallint NOT NULL, -- 0 user,  1 group
    key    text     NOT NULL
);

CREATE INDEX document_ref_doc ON document_ref (doc_id, effect, scope);
CREATE INDEX document_ref_key ON document_ref (key, effect, scope);

-- document_data is the document itself, as the JSON that Put was handed.
--
-- It is a separate table because of the shape of the reads. The columns above
-- are what every query filters, counts and ranks on, and they are narrow enough
-- that a page holds hundreds of them. The document averages a few kilobytes, so
-- keeping it in the same row would push almost every row onto a TOAST chain and
-- make the candidate cut pay for bodies it never reads.
--
-- It is text rather than jsonb because nothing queries into it. jsonb would
-- parse every document on the way in and rebuild it on the way out to store the
-- same bytes, and an operator who wants to look inside one can still write
-- data::jsonb.
CREATE TABLE document_data (
    doc_id text NOT NULL PRIMARY KEY REFERENCES document (id) ON DELETE CASCADE,
    data   text NOT NULL
);

-- document_content is the bytes of a document that is not text. Same argument
-- as document_data, one level further in: only the endpoint that serves an
-- image ever reads it.
CREATE TABLE document_content (
    doc_id text    NOT NULL PRIMARY KEY REFERENCES document (id) ON DELETE CASCADE,
    width  integer NOT NULL,
    height integer NOT NULL,
    bytes  bytea   NOT NULL
);

-- posting is the term frequency store, not a second matcher. The tsvector stays
-- the thing that decides which documents are candidates.
--
-- The key is (doc_id, term) rather than (term, doc_id), which is the other way
-- round from a classic inverted index. Nothing here ever asks which documents
-- carry a term: that is answered by the tsvector for matching and by term_stat
-- for document frequency. What it does ask is which terms a handful of already
-- chosen candidates carry, and this order answers that from the primary key.
--
-- The key is the document id rather than a surrogate integer. A surrogate would
-- be a narrower index, and it would also have to be decided before the document
-- row is written, which is a read modify write that two servers writing the
-- same document can interleave. Correctness first: pgstore is the driver people
-- run because they must, and it is measured against a bitmap engine that it is
-- never going to beat on index size.
CREATE TABLE posting (
    doc_id   text    NOT NULL REFERENCES document (id) ON DELETE CASCADE,
    term     text    NOT NULL,
    title_tf integer NOT NULL,
    body_tf  integer NOT NULL,
    PRIMARY KEY (doc_id, term)
);

-- corpus and term_stat are the numbers BM25 needs about the corpus rather than
-- about a document. They are maintained on every write so that a query reads a
-- row instead of running an aggregate over everything.
CREATE TABLE corpus (
    tenant       text   NOT NULL PRIMARY KEY,
    documents    bigint NOT NULL,
    title_tokens bigint NOT NULL,
    body_tokens  bigint NOT NULL
);

CREATE TABLE term_stat (
    tenant    text   NOT NULL,
    term      text   NOT NULL,
    documents bigint NOT NULL,
    PRIMARY KEY (tenant, term)
);
