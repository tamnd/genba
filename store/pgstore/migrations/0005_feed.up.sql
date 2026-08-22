-- feed is how the connectors were configured, so that a connector somebody
-- added from the interface is still there after a restart.
--
-- It is the first table in this schema that is not a document or derived from
-- one, and it is the only one with no reference to document on purpose. A feed
-- and the documents it produced are joinable by (tenant, source) and are
-- deliberately not tied together: dropping a feed forgets how a corpus was read
-- and leaves the corpus, because the alternative makes an operator's undo cost
-- a full crawl, and on a large source that is hours.
--
-- It is also the reason this table exists in the database rather than in a file
-- next to it. A file works on a laptop and quietly gives three servers behind a
-- load balancer three different sets of connectors.
CREATE TABLE feed (
    tenant     text    NOT NULL,
    source     text    NOT NULL,
    kind       text    NOT NULL,
    enabled    boolean NOT NULL,

    -- The connector's own settings, as JSON this schema never reads. A bucket
    -- has an endpoint and a region, a directory has a path and a policy, and a
    -- column per setting would be a migration every time a connector grew a
    -- field. It is text rather than jsonb for the same reason document_data is:
    -- it is handed back byte for byte and nothing queries inside it.
    --
    -- Credentials are not in here and are not anywhere in this database. A
    -- database is backed up, replicated and read by more people than a process
    -- environment is, and a secret that reaches one of those places cannot be
    -- recalled from it.
    config     text    NOT NULL,

    -- Who last wrote the row, which is the question that gets asked when two
    -- operators are changing the same deployment.
    by_subject text    NOT NULL,

    -- Unix nanoseconds, for the same reason document.modified_at is: a
    -- timestamptz keeps microseconds, and a time read back out of one does not
    -- compare equal to the time that went in.
    created    bigint  NOT NULL,
    updated    bigint  NOT NULL,

    -- Two tenants may both have a source called files, and one tenant may not
    -- have two.
    PRIMARY KEY (tenant, source)
);
