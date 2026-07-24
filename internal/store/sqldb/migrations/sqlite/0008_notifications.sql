-- 0008_notifications: Phase 5 delivery state.
--
-- Two things: where a person wants notifications, and which ones have already
-- been sent. Neither touches the ledger -- notifications are entirely off the
-- balance path, which is the load-bearing rule of the phase.
--
-- ---------------------------------------------------------------------------
-- Per-user delivery preferences.
--
-- ntfy_topic is the only user-controlled part of an ntfy destination (the
-- server is admin-pinned, decision D1). It is validated to a strict charset by
-- notify.ValidTopic before it is stored and again before it is used.
--
-- The channel toggles default to email on, ntfy off: email needs only the
-- address the account already has, while ntfy needs a topic the user has not
-- set yet. Empty string / 0 is "off", matching every other optional column
-- here (no NULLs to COALESCE, DEPLOY-02).
-- ---------------------------------------------------------------------------
ALTER TABLE users ADD COLUMN ntfy_topic    TEXT    NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN notify_email  INTEGER NOT NULL DEFAULT 1 CHECK (notify_email IN (0, 1));
ALTER TABLE users ADD COLUMN notify_ntfy   INTEGER NOT NULL DEFAULT 0 CHECK (notify_ntfy IN (0, 1));

-- ---------------------------------------------------------------------------
-- sent_notifications: the send-once claim table.
--
-- The fourth sibling of posted_periods / posted_fees / posted_interest, and the
-- same (scope, key) primary key makes a send happen at most once. But the
-- guarantee is DIFFERENT and weaker on purpose: a network send cannot join a
-- database transaction, so this is at-LEAST-once (decision D2), not the ledger's
-- exactly-once. The claim is written AFTER a confirmed successful send, in its
-- own short transaction, never inside a ledger transaction -- so a send failure
-- can never roll back a financial entry, and a crash between send and claim
-- re-sends one harmless duplicate rather than dropping the notice that matters.
--
-- event_key identifies one notifiable event for one recipient, e.g.
-- "req:2026-08-01:u7" (a two-week payment request for the Aug 1 period, to user
-- 7). It is built by the caller and opaque here.
--
-- channel records which method delivered it, so a person who has both email and
-- ntfy on is not notified twice for the same event on the same channel while a
-- re-run can still reach the other channel if it had failed.
-- ---------------------------------------------------------------------------
CREATE TABLE sent_notifications (
    tab_id     INTEGER NOT NULL REFERENCES tabs (id) ON DELETE CASCADE,
    event_key  TEXT    NOT NULL,
    channel    TEXT    NOT NULL,
    user_id    INTEGER NOT NULL REFERENCES users (id),
    sent_at    TEXT    NOT NULL,
    PRIMARY KEY (tab_id, event_key, channel)
);

CREATE INDEX idx_sent_notifications_user ON sent_notifications (user_id);

-- Append-only, like every accrual claim: a sent notification is a fact, and
-- rewriting or deleting one would let it send again.
CREATE TRIGGER sent_notifications_no_update
BEFORE UPDATE ON sent_notifications
BEGIN
    SELECT RAISE(ABORT, 'sent notifications are append-only: UPDATE is not permitted');
END;

CREATE TRIGGER sent_notifications_no_delete
BEFORE DELETE ON sent_notifications
BEGIN
    SELECT RAISE(ABORT, 'sent notifications are append-only: DELETE is not permitted');
END;
