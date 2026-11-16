-- Reading note: this is the original schema. Migration 002 replaces the
-- check_journal validation triggers with a deduplicated deferred-check queue.
-- This annotation pass changes comments only; executable migration SQL is fixed.
-- SQL constraints apply to ordinary direct SQL writes as well as Go calls.
-- Privileged owners can disable/change these mechanisms; this is not an access
-- control boundary against a database administrator.

-- accounts.balance is a synchronous projection of entries, maintained below by
-- apply_entry in the same transaction. It is not independent funding data.
CREATE TABLE accounts (
 -- Identity sequences can have gaps after rollback; IDs do not imply commit order.
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 code text NOT NULL UNIQUE CHECK (length(code) BETWEEN 1 AND 100),
 name text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
 kind text NOT NULL CHECK (kind IN ('asset','liability','equity','income','expense')),
 currency text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
 normal_side text NOT NULL CHECK (normal_side IN ('debit','credit')),
 allow_overdraft boolean NOT NULL DEFAULT false,
 balance bigint NOT NULL DEFAULT 0,
 -- This CHECK runs on each balance update, explaining increases-first in Go.
 CHECK (allow_overdraft OR balance >= 0)
);
-- A journal header and its key are inserted in the posting transaction. UNIQUE
-- arbitrates concurrent retries; rolling back also releases the key claim.
CREATE TABLE journals (
 id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 idempotency_key text NOT NULL UNIQUE CHECK (length(idempotency_key) BETWEEN 1 AND 200),
 request_hash bytea NOT NULL,
 description text NOT NULL CHECK (length(description) <= 1000),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 -- Remember the inserting transaction so later transactions cannot append lines.
 posting_xid xid8 NOT NULL DEFAULT pg_current_xact_id()
);
-- Lines are the immutable accounting record. Foreign keys require real parent
-- accounts/journals, and (journal_id,line) prevents duplicate line numbers.
CREATE TABLE entries (
 journal_id bigint NOT NULL REFERENCES journals(id),
 line integer NOT NULL CHECK (line > 0),
 account_id bigint NOT NULL REFERENCES accounts(id),
 side text NOT NULL CHECK (side IN ('debit','credit')),
 amount bigint NOT NULL CHECK (amount > 0),
 PRIMARY KEY (journal_id,line)
);
-- Support account-based history/reconciliation without scanning every entry.
CREATE INDEX entries_account ON entries(account_id,journal_id);
-- A trigger is database code run by a write. BEFORE can reject the write;
-- AFTER runs once the row was inserted. ERRCODE is the SQLSTATE mapped by HTTP.
CREATE FUNCTION immutable_row() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'posted journal data is immutable' USING ERRCODE='23514'; END $$;
CREATE TRIGGER journals_immutable BEFORE UPDATE OR DELETE ON journals FOR EACH ROW EXECUTE FUNCTION immutable_row();
CREATE TRIGGER entries_immutable BEFORE UPDATE OR DELETE ON entries FOR EACH ROW EXECUTE FUNCTION immutable_row();
-- NEW is the proposed/inserted row supplied by PostgreSQL to the trigger.
-- This function is called by entries_apply; Go does not call it separately.
CREATE FUNCTION apply_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE x xid8;
BEGIN
 -- Lock the parent and load its creator transaction ID into the local variable x.
 SELECT posting_xid INTO x FROM journals WHERE id=NEW.journal_id FOR UPDATE;
 IF x IS DISTINCT FROM pg_current_xact_id() THEN
  RAISE EXCEPTION 'cannot append to committed journal' USING ERRCODE='23514';
 END IF;
 -- Atomic database arithmetic uses the current row value. PostgreSQL checks
 -- bigint overflow and the overdraft CHECK; any failure aborts this statement.
 UPDATE accounts SET balance = balance + CASE WHEN normal_side=NEW.side THEN NEW.amount ELSE -NEW.amount END WHERE id=NEW.account_id;
 RETURN NEW;
END $$;
-- AFTER INSERT matters: a line skipped by ON CONFLICT DO NOTHING must not move
-- money. A BEFORE INSERT balance side effect could run even for a skipped line.
CREATE TRIGGER entries_apply AFTER INSERT ON entries FOR EACH ROW EXECUTE FUNCTION apply_entry();
-- Original v1 validation: one aggregate check was scheduled for every header and
-- line, repeatedly scanning the same journal. Read 002 for the active v2 approach.
CREATE FUNCTION check_journal() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE n bigint; total numeric; currencies bigint; journal bigint;
BEGIN
 IF TG_TABLE_NAME='journals' THEN journal := NEW.id; ELSE journal := NEW.journal_id; END IF;
 -- numeric avoids int64 overflow in totals; coalesce handles an empty journal.
 -- Signed debit-minus-credit sum zero means balanced, regardless of normal sides.
 SELECT count(*),coalesce(sum(CASE WHEN e.side='debit' THEN e.amount::numeric ELSE -e.amount::numeric END),0),count(DISTINCT a.currency)
 INTO n,total,currencies FROM entries e JOIN accounts a ON a.id=e.account_id WHERE e.journal_id=journal;
 IF n < 2 OR total <> 0 OR currencies <> 1 THEN
 RAISE EXCEPTION 'journal must have at least two balanced entries in one currency' USING ERRCODE='23514';
 END IF;
 RETURN NULL;
END $$;
-- Deferred means normally checked at COMMIT, when all lines are available.
-- A single unfinished line may be unbalanced; a committed journal may not be.
CREATE CONSTRAINT TRIGGER journals_balanced AFTER INSERT ON journals DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_journal();
-- Also schedule validation on each entry: SET CONSTRAINTS IMMEDIATE followed by
-- more entries must not permit an unbalanced commit.
CREATE CONSTRAINT TRIGGER entries_balanced AFTER INSERT ON entries DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION check_journal();
-- Account identity and policy are fixed in v1; balance is maintained by entry triggers.
CREATE FUNCTION protect_account() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 -- OLD/NEW let us detect changes to policy or identity. TG_OP is the operation.
 -- A normal direct UPDATE has trigger depth 1; apply_entry's nested account
 -- update reaches depth 2. This guard assumes trusted schema/trigger ownership.
 IF TG_OP='DELETE' OR NEW.id<>OLD.id OR NEW.code<>OLD.code OR NEW.name<>OLD.name OR NEW.kind<>OLD.kind OR NEW.currency<>OLD.currency OR NEW.normal_side<>OLD.normal_side OR NEW.allow_overdraft<>OLD.allow_overdraft OR pg_trigger_depth()<2 THEN
 RAISE EXCEPTION 'account changes are not supported' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER accounts_protected BEFORE UPDATE OR DELETE ON accounts FOR EACH ROW EXECUTE FUNCTION protect_account();
-- Even direct SQL must start accounts at zero. Opening funds need entries so
-- a balance can always be reconciled back to the accounting record.
CREATE FUNCTION zero_opening_balance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.balance <> 0 THEN RAISE EXCEPTION 'opening balances require a journal' USING ERRCODE='23514'; END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER accounts_zero BEFORE INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION zero_opening_balance();
