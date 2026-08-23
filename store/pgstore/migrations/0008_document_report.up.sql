-- document_report is a reader saying that a document is out of date.
--
-- It is the other half of document_verify and the cheaper half. Verifying costs
-- the person who is accountable half an hour of reading; reporting costs the
-- person who just lost an hour to a stale runbook about ten seconds, and it is
-- the moment they are most willing to spend them.
--
-- One row per person per document rather than one per click, which is the shape
-- document_open has and is what makes the number under a document worth
-- printing: it is the count of people who said so. Somebody reporting the same
-- document twice found it stale again rather than complaining twice, so the
-- second report lands on the first.
--
-- The cascade is the promise every table beside document makes here. A document
-- that leaves the corpus takes what was said about it, so a document re-crawled
-- under an id that was deleted does not arrive already complained about.
CREATE TABLE document_report (
    doc_id      text   NOT NULL REFERENCES document(id) ON DELETE CASCADE,

    -- by_key is store.ReportKey: the principal's own subject, folded, so that
    -- the same person reporting from two sessions is one person rather than
    -- two. It is the key and not a name, because a name changes and a count
    -- that moves when somebody gets married is not a count.
    by_key      text   NOT NULL,

    -- Who they are, carried whole, so that the owner reading the report
    -- recognises a name without a second lookup. JSON in one column rather than
    -- three, for the reason document_own stores its people that way: a person
    -- flattened into a subject, a name and an address comes back missing the
    -- source identity. Text rather than jsonb, again like document_own, because
    -- nothing queries into it.
    reporter    text   NOT NULL,

    -- Unix nanoseconds, for the same reason document_verify.verified_at is: a
    -- timestamptz keeps microseconds, and a time read back out of one does not
    -- compare equal to the time that went in.
    reported_at bigint NOT NULL,

    -- What is wrong, in their words, and the reason this is worth more than a
    -- counter. Optional, because a report nobody could be bothered to write a
    -- sentence for is still a report and refusing it would trade the signal for
    -- the sentence.
    note        text   NOT NULL,

    PRIMARY KEY (doc_id, by_key)
);

-- The inbox reads this newest first and then asks which of those documents
-- belong to the person asking, so the date is the column it walks. The primary
-- key answers the other question, what has been said about these twenty ids,
-- from a range scan per id.
CREATE INDEX document_report_recent ON document_report (reported_at DESC);
