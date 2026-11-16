-- Queue one full validation per journal between constraint flushes. Queue rows
-- cannot survive a successful commit. Never change the accounting record here.
-- This is a normal table whose marker writes participate in the posting
-- transaction, not an external job queue or background worker.
CREATE TABLE pending_journals (
 journal_id bigint PRIMARY KEY REFERENCES journals(id)
);
CREATE FUNCTION protect_pending_journal() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- Ordinary SQL must not remove a pending check or invent markers. Nested
 -- accounting triggers maintain this table; privileged schema owners are trusted.
 IF TG_OP='UPDATE' OR pg_trigger_depth()<2 THEN
  RAISE EXCEPTION 'journal validation queue is internal' USING ERRCODE='23514';
 END IF;
 IF TG_OP='DELETE' THEN RETURN OLD; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER pending_journals_protected BEFORE INSERT OR UPDATE OR DELETE ON pending_journals
 FOR EACH ROW EXECUTE FUNCTION protect_pending_journal();

-- Header insertion marks even an empty journal, so empty commits are checked.
-- Every new line also marks it: appending after an early constraint flush must
-- schedule a fresh check. The primary key deduplicates repeated marks.
CREATE FUNCTION mark_journal_pending() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE journal bigint;
BEGIN
 IF TG_TABLE_NAME='journals' THEN journal := NEW.id; ELSE journal := NEW.journal_id; END IF;
 INSERT INTO pending_journals(journal_id) VALUES(journal) ON CONFLICT DO NOTHING;
 RETURN NULL;
END $$;
-- Runs when deferred constraints are flushed, normally at COMMIT. Aggregate all
-- lines once for this dirty journal; numeric permits totals larger than int64.
CREATE FUNCTION validate_pending_journal() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE n bigint; total numeric; currencies bigint;
BEGIN
 SELECT count(*),coalesce(sum(CASE WHEN e.side='debit' THEN e.amount::numeric ELSE -e.amount::numeric END),0),count(DISTINCT a.currency)
 INTO n,total,currencies FROM entries e JOIN accounts a ON a.id=e.account_id WHERE e.journal_id=NEW.journal_id;
 IF n < 2 OR total <> 0 OR currencies <> 1 THEN
  RAISE EXCEPTION 'journal must have at least two balanced entries in one currency' USING ERRCODE='23514';
 END IF;
 -- A successful check consumes the marker. If more lines arrive later in this
 -- transaction, marking inserts a new row and schedules another check.
 -- A savepoint rollback restores marker changes and pending checks together.
 DELETE FROM pending_journals WHERE journal_id=NEW.journal_id;
 RETURN NULL;
END $$;
-- Replace the old repeated full scans, preserving journal/account constraints
-- and balance updates from 001. This upgrade is applied atomically by Apply.
DROP TRIGGER journals_balanced ON journals;
DROP TRIGGER entries_balanced ON entries;
DROP FUNCTION check_journal();
CREATE TRIGGER journals_pending AFTER INSERT ON journals FOR EACH ROW EXECUTE FUNCTION mark_journal_pending();
CREATE TRIGGER entries_pending AFTER INSERT ON entries FOR EACH ROW EXECUTE FUNCTION mark_journal_pending();
-- SET CONSTRAINTS ALL IMMEDIATE can run the check earlier. Subsequent writes
-- still need validation; the marker lifecycle above is what makes that safe.
CREATE CONSTRAINT TRIGGER pending_journals_balanced AFTER INSERT ON pending_journals
 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION validate_pending_journal();
