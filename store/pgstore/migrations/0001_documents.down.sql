-- Dropping in dependency order rather than relying on CASCADE, so that a
-- rollback that finds something unexpected attached to one of these tables
-- fails and says so instead of quietly taking it with them.
DROP TABLE term_stat;
DROP TABLE corpus;
DROP TABLE posting;
DROP TABLE document_content;
DROP TABLE document_data;
DROP TABLE document_ref;
DROP TABLE document;
